package main

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/flynn/flynn/appliance/mariadb"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/provider"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/shutdown"
	sirenia "github.com/flynn/flynn/pkg/sirenia/client"
	"github.com/flynn/flynn/pkg/sirenia/scale"
	"github.com/flynn/flynn/pkg/sirenia/state"
	"github.com/flynn/flynn/pkg/status/protobuf"
	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"gopkg.in/inconshreveable/log15.v2"
)

var serviceName = os.Getenv("FLYNN_MYSQL")
var app = os.Getenv("FLYNN_APP_ID")
var controllerKey = os.Getenv("CONTROLLER_KEY")
var singleton = os.Getenv("SINGLETON")
var serviceHost string

func init() {
	if serviceName == "" {
		serviceName = "mariadb"
	}
	serviceHost = fmt.Sprintf("leader.%s.discoverd", serviceName)
}

func main() {
	defer shutdown.Exit()

	s := grpc.NewServer()
	rpcServer := &API{}
	provider.RegisterProviderServer(s, rpcServer)
	status.RegisterStatusServer(s, rpcServer)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port

	l, err := net.Listen("tcp", addr)
	if err != nil {
		shutdown.Fatal(err)
	}

	hb, err := discoverd.AddServiceAndRegister(serviceName+"-api", addr)
	if err != nil {
		shutdown.Fatal(err)
	}
	shutdown.BeforeExit(func() { hb.Close() })

	shutdown.Fatal(s.Serve(l))
}

// API implements the Provider and Status services
type API struct {
	mtx      sync.Mutex
	scaledUp bool
}

func (a *API) Provision(ctx context.Context, _ *provider.ProvisionRequest) (*provider.ProvisionReply, error) {
	// Ensure the cluster has been scaled up before attempting to create a database.
	if err := a.scaleUp(); err != nil {
		return nil, err
	}

	db, err := a.connect()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	username, password, database := random.Hex(16), random.Hex(16), random.Hex(16)
	if _, err := db.Exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", username, password)); err != nil {
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE `%s`", database)); err != nil {
		db.Exec(fmt.Sprintf("DROP USER '%s'", username))
		return nil, err
	}
	if _, err := db.Exec(fmt.Sprintf("GRANT ALL ON `%s`.* TO '%s'@'%%'", database, username)); err != nil {
		db.Exec(fmt.Sprintf("DROP DATABASE `%s`", database))
		db.Exec(fmt.Sprintf("DROP USER '%s'", username))
		return nil, err
	}

	url := fmt.Sprintf("mysql://%s:%s@%s:3306/%s", username, password, serviceHost, database)
	return &provider.ProvisionReply{
		Id: fmt.Sprintf("/databases/%s:%s", username, database),
		Env: map[string]string{
			"FLYNN_MYSQL":    serviceName,
			"MYSQL_HOST":     serviceHost,
			"MYSQL_USER":     username,
			"MYSQL_PWD":      password,
			"MYSQL_DATABASE": database,
			"DATABASE_URL":   url,
		},
	}, nil
}

func (a *API) Deprovision(ctx context.Context, req *provider.DeprovisionRequest) (*provider.DeprovisionReply, error) {
	reply := &provider.DeprovisionReply{}
	id := strings.SplitN(strings.TrimPrefix(req.Id, "/databases/"), ":", 2)
	if len(id) != 2 || id[1] == "" {
		return reply, fmt.Errorf("id is invalid")
	}

	db, err := a.connect()
	if err != nil {
		return reply, err
	}
	defer db.Close()

	if _, err := db.Exec(fmt.Sprintf("DROP DATABASE `%s`", id[1])); err != nil {
		return reply, err
	}

	if _, err := db.Exec(fmt.Sprintf("DROP USER '%s'", id[0])); err != nil {
		return reply, err
	}
	return reply, nil
}

func (a *API) GetTunables(ctx context.Context, req *provider.GetTunablesRequest) (*provider.GetTunablesReply, error) {
	reply := &provider.GetTunablesReply{}
	sc := sirenia.NewClient(serviceHost + ":3306")
	tunables, err := sc.GetTunables()
	if err != nil {
		return reply, err
	}
	reply.Tunables = tunables.Data
	reply.Version = tunables.Version
	return reply, nil
}

func (a *API) UpdateTunables(ctx context.Context, req *provider.UpdateTunablesRequest) (*provider.UpdateTunablesReply, error) {
	reply := &provider.UpdateTunablesReply{}
	if singleton == "true" {
		return reply, fmt.Errorf("Tunables can't be updated on singleton clusters")
	}
	update := &state.Tunables{
		Data:    req.Tunables,
		Version: req.Version,
	}
	sc := sirenia.NewClient(serviceHost + ":3306")
	err := sc.UpdateTunables(update)
	return reply, err
}

// TODO(jpg): refactor
func (a *API) Status(ctx context.Context, _ *status.StatusRequest) (*status.StatusReply, error) {
	logger := a.logger().New("fn", "Status")

	logger.Info("checking status", "host", serviceHost)
	if ss, err := sirenia.NewClient(serviceHost + ":3306").Status(); err == nil && ss.Database != nil && ss.Database.ReadWrite {
		logger.Info("database is up, skipping scale check")
	} else {
		scaled, err := scale.CheckScale(app, controllerKey, "mariadb", a.logger())
		if err != nil {
			return &status.StatusReply{
				Status: status.StatusReply_UNHEALTHY,
			}, err
		}

		// Cluster has yet to be scaled, return healthy
		if !scaled {
			return &status.StatusReply{}, nil
		}
	}

	db, err := a.connect()
	if err != nil {
		return &status.StatusReply{
			Status: status.StatusReply_UNHEALTHY,
		}, err
	}
	defer db.Close()

	if _, err := db.Exec("SELECT 1"); err != nil {
		return &status.StatusReply{
			Status: status.StatusReply_UNHEALTHY,
		}, err
	}
	return &status.StatusReply{}, nil
}

func (a *API) connect() (*sql.DB, error) {
	dsn := &mariadb.DSN{
		Host:     serviceHost + ":3306",
		User:     "flynn",
		Password: os.Getenv("MYSQL_PWD"),
		Database: "mysql",
	}
	return sql.Open("mysql", dsn.String())
}

func (a *API) scaleUp() error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	// Ignore if already scaled up.
	if a.scaledUp {
		return nil
	}

	serviceAddr := serviceHost + ":3306"
	err := scale.ScaleUp(app, controllerKey, serviceAddr, "mariadb", singleton, a.logger())
	if err != nil {
		return err
	}

	// Mark as successfully scaled up.
	a.scaledUp = true
	return nil
}

func (a *API) logger() log15.Logger {
	return log15.New("app", "mariadb-web")
}
