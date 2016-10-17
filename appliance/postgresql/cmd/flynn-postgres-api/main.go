package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/provider"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/shutdown"
	sirenia "github.com/flynn/flynn/pkg/sirenia/client"
	"github.com/flynn/flynn/pkg/sirenia/state"
	"github.com/flynn/flynn/pkg/status/protobuf"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

const (
	disallowConns   = `UPDATE pg_database SET datallowconn = FALSE WHERE datname = $1`
	disconnectConns = `
SELECT pg_terminate_backend(pg_stat_activity.pid)
FROM pg_stat_activity
WHERE pg_stat_activity.datname = $1
  AND pid <> pg_backend_pid();`
)

var serviceName = os.Getenv("FLYNN_POSTGRES")
var singleton = os.Getenv("SINGLETON")
var serviceHost string

func init() {
	if serviceName == "" {
		serviceName = "postgres"
	}
	serviceHost = fmt.Sprintf("leader.%s.discoverd", serviceName)
}

func main() {
	defer shutdown.Exit()

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port

	db := postgres.Wait(&postgres.Conf{
		Service:  serviceName,
		User:     "flynn",
		Password: os.Getenv("PGPASSWORD"),
		Database: "postgres",
	}, nil)
	s := grpc.NewServer()
	rpcServer := &server{db}
	provider.RegisterProviderServer(s, rpcServer)
	status.RegisterStatusServer(s, rpcServer)

	l, err := net.Listen("tcp", ":"+port)
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

// server implements the Provider and Status services
type server struct {
	db *postgres.DB
}

func (p *server) Provision(ctx context.Context, _ *provider.ProvisionRequest) (*provider.ProvisionReply, error) {
	username, password, database := random.Hex(16), random.Hex(16), random.Hex(16)

	if err := p.db.Exec(fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '%s'`, username, password)); err != nil {
		return nil, err
	}
	if err := p.db.Exec(fmt.Sprintf(`CREATE DATABASE "%s"`, database)); err != nil {
		p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, username))
		return nil, err
	}
	if err := p.db.Exec(fmt.Sprintf(`GRANT ALL ON DATABASE "%s" TO "%s"`, database, username)); err != nil {
		p.db.Exec(fmt.Sprintf(`DROP DATABASE "%s"`, database))
		p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, username))
		return nil, err
	}

	url := fmt.Sprintf("postgres://%s:%s@%s:5432/%s", username, password, serviceHost, database)
	return &provider.ProvisionReply{
		Id: fmt.Sprintf("/databases/%s:%s", username, database),
		Env: map[string]string{
			"FLYNN_POSTGRES": serviceName,
			"PGHOST":         serviceHost,
			"PGUSER":         username,
			"PGPASSWORD":     password,
			"PGDATABASE":     database,
			"DATABASE_URL":   url,
		},
	}, nil
}

func (p *server) Deprovision(ctx context.Context, req *provider.DeprovisionRequest) (*provider.DeprovisionReply, error) {
	reply := &provider.DeprovisionReply{}
	id := strings.SplitN(strings.TrimPrefix(req.Id, "/databases/"), ":", 2)
	if len(id) != 2 || id[1] == "" {
		return reply, fmt.Errorf("id is invalid")
	}

	// disable new connections to the target database
	if err := p.db.Exec(disallowConns, id[1]); err != nil {
		return reply, err
	}

	// terminate current connections
	if err := p.db.Exec(disconnectConns, id[1]); err != nil {
		return reply, err
	}

	if err := p.db.Exec(fmt.Sprintf(`DROP DATABASE "%s"`, id[1])); err != nil {
		return reply, err
	}

	if err := p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, id[0])); err != nil {
		return reply, err
	}
	return reply, nil
}

func (p *server) GetTunables(ctx context.Context, req *provider.GetTunablesRequest) (*provider.GetTunablesReply, error) {
	reply := &provider.GetTunablesReply{}
	sc := sirenia.NewClient(serviceHost + ":5432")
	tunables, err := sc.GetTunables()
	if err != nil {
		return reply, err
	}
	reply.Tunables = tunables.Data
	reply.Version = tunables.Version
	return reply, nil
}

func (p *server) UpdateTunables(ctx context.Context, req *provider.UpdateTunablesRequest) (*provider.UpdateTunablesReply, error) {
	reply := &provider.UpdateTunablesReply{}
	if singleton == "true" {
		return reply, fmt.Errorf("Tunables can't be updated on singleton clusters")
	}
	update := &state.Tunables{
		Data:    req.Tunables,
		Version: req.Version,
	}
	sc := sirenia.NewClient(serviceHost + ":5432")
	err := sc.UpdateTunables(update)
	return reply, err
}

func (p *server) Status(ctx context.Context, _ *status.StatusRequest) (*status.StatusReply, error) {
	if err := p.db.Exec("SELECT 1"); err != nil {
		return &status.StatusReply{
			Status: status.StatusReply_UNHEALTHY,
		}, err
	}
	return &status.StatusReply{}, nil
}
