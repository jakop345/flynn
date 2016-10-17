package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/provider"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/shutdown"
	sirenia "github.com/flynn/flynn/pkg/sirenia/client"
	"github.com/flynn/flynn/pkg/sirenia/scale"
	"github.com/flynn/flynn/pkg/sirenia/state"
	"github.com/flynn/flynn/pkg/status/protobuf"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"gopkg.in/inconshreveable/log15.v2"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var app = os.Getenv("FLYNN_APP_ID")
var controllerKey = os.Getenv("CONTROLLER_KEY")
var singleton = os.Getenv("SINGLETON")
var serviceName = os.Getenv("FLYNN_MONGO")
var serviceHost string

func init() {
	if serviceName == "" {
		serviceName = "mongodb"
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

func (a *API) logger() log15.Logger {
	return log15.New("app", "mongodb-web")
}

func (a *API) Provision(ctx context.Context, _ *provider.ProvisionRequest) (*provider.ProvisionReply, error) {
	// Ensure the cluster has been scaled up before attempting to create a database.
	if err := a.scaleUp(); err != nil {
		return nil, err
	}

	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: "admin",
	})
	if err != nil {
		return nil, err
	}
	defer session.Close()

	username, password, database := random.Hex(16), random.Hex(16), random.Hex(16)

	// Create a user
	if err := session.DB(database).Run(bson.D{
		{"createUser", username},
		{"pwd", password},
		{"roles", []bson.M{
			{"role": "dbOwner", "db": database},
		}},
	}, nil); err != nil {
		return nil, err
	}

	url := fmt.Sprintf("mongodb://%s:%s@%s:27017/%s", username, password, serviceHost, database)
	return &provider.ProvisionReply{
		Id: fmt.Sprintf("/databases/%s:%s", username, database),
		Env: map[string]string{
			"FLYNN_MONGO":    serviceName,
			"MONGO_HOST":     serviceHost,
			"MONGO_USER":     username,
			"MONGO_PWD":      password,
			"MONGO_DATABASE": database,
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
	user, database := id[0], id[1]

	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: "admin",
	})
	if err != nil {
		return reply, err
	}
	defer session.Close()

	// Delete user.
	if err := session.DB(database).Run(bson.D{{"dropUser", user}}, nil); err != nil {
		return reply, err
	}

	// Delete database.
	if err := session.DB(database).Run(bson.D{{"dropDatabase", 1}}, nil); err != nil {
		return reply, err
	}

	return reply, nil
}

func (a *API) GetTunables(ctx context.Context, req *provider.GetTunablesRequest) (*provider.GetTunablesReply, error) {
	reply := &provider.GetTunablesReply{}
	sc := sirenia.NewClient(serviceHost + ":27017")
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
	sc := sirenia.NewClient(serviceHost + ":27017")
	err := sc.UpdateTunables(update)
	return reply, err
}

// TODO(jpg): refactor
func (a *API) Status(ctx context.Context, _ *status.StatusRequest) (*status.StatusReply, error) {
	logger := a.logger().New("fn", "Status")

	logger.Info("checking status", "host", serviceHost)
	if ss, err := sirenia.NewClient(serviceHost + ":27017").Status(); err == nil && ss.Database != nil && ss.Database.ReadWrite {
		logger.Info("database is up, skipping scale check")
	} else {
		scaled, err := scale.CheckScale(app, controllerKey, "mongodb", a.logger())
		if err != nil {
			return &status.StatusReply{Status: status.StatusReply_UNHEALTHY}, err
		}

		// Cluster has yet to be scaled, return healthy
		if !scaled {
			return &status.StatusReply{}, nil
		}
	}

	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: "admin",
	})
	if err != nil {
		return &status.StatusReply{Status: status.StatusReply_UNHEALTHY}, err
	}
	defer session.Close()

	return &status.StatusReply{}, nil
}

func (a *API) scaleUp() error {
	a.mtx.Lock()
	defer a.mtx.Unlock()

	// Ignore if already scaled up.
	if a.scaledUp {
		return nil
	}

	serviceAddr := serviceHost + ":27017"
	err := scale.ScaleUp(app, controllerKey, serviceAddr, "mongodb", singleton, a.logger())
	if err != nil {
		return err
	}

	// Mark as successfully scaled up.
	a.scaledUp = true
	return nil
}
