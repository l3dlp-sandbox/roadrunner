package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spiral/errors"
	"github.com/spiral/roadrunner/v2"
	"github.com/spiral/roadrunner/v2/interfaces/log"
	"github.com/spiral/roadrunner/v2/interfaces/server"
	"github.com/spiral/roadrunner/v2/plugins/config"
	"github.com/spiral/roadrunner/v2/util"
)

const ServiceName = "server"

// Plugin manages worker
type Plugin struct {
	cfg     Config
	log     log.Logger
	factory roadrunner.Factory
}

// Init application provider.
func (server *Plugin) Init(cfg config.Configurer, log log.Logger) error {
	const op = errors.Op("Init")
	err := cfg.UnmarshalKey(ServiceName, &server.cfg)
	if err != nil {
		return errors.E(op, errors.Init, err)
	}
	server.cfg.InitDefaults()
	server.log = log

	server.factory, err = server.initFactory()
	if err != nil {
		return errors.E(errors.Op("Init factory"), err)
	}

	return nil
}

// Name contains service name.
func (server *Plugin) Name() string {
	return ServiceName
}

func (server *Plugin) Serve() chan error {
	errCh := make(chan error, 1)
	return errCh
}

func (server *Plugin) Stop() error {
	if server.factory == nil {
		return nil
	}

	return server.factory.Close(context.Background())
}

// CmdFactory provides worker command factory assocated with given context.
func (server *Plugin) CmdFactory(env server.Env) (func() *exec.Cmd, error) {
	const op = errors.Op("cmd factory")
	var cmdArgs []string

	// create command according to the config
	cmdArgs = append(cmdArgs, strings.Split(server.cfg.Command, " ")...)
	if len(cmdArgs) < 2 {
		return nil, errors.E(op, errors.Str("should be in form of `php <script>"))
	}
	if cmdArgs[0] != "php" {
		return nil, errors.E(op, errors.Str("first arg in command should be `php`"))
	}
	return func() *exec.Cmd {
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...) //nolint:gosec
		util.IsolateProcess(cmd)

		// if user is not empty, and OS is linux or macos
		// execute php worker from that particular user
		if server.cfg.User != "" {
			err := util.ExecuteFromUser(cmd, server.cfg.User)
			if err != nil {
				return nil
			}
		}

		cmd.Env = server.setEnv(env)

		return cmd
	}, nil
}

// NewWorker issues new standalone worker.
func (server *Plugin) NewWorker(ctx context.Context, env server.Env) (roadrunner.WorkerBase, error) {
	const op = errors.Op("new worker")
	spawnCmd, err := server.CmdFactory(env)
	if err != nil {
		return nil, errors.E(op, err)
	}

	w, err := server.factory.SpawnWorkerWithContext(ctx, spawnCmd())
	if err != nil {
		return nil, errors.E(op, err)
	}

	w.AddListener(server.collectLogs)

	return w, nil
}

// NewWorkerPool issues new worker pool.
func (server *Plugin) NewWorkerPool(ctx context.Context, opt roadrunner.PoolConfig, env server.Env) (roadrunner.Pool, error) {
	spawnCmd, err := server.CmdFactory(env)
	if err != nil {
		return nil, err
	}

	p, err := roadrunner.NewPool(ctx, spawnCmd, server.factory, opt)
	if err != nil {
		return nil, err
	}

	p.AddListener(server.collectLogs)

	return p, nil
}

// creates relay and worker factory.
func (server *Plugin) initFactory() (roadrunner.Factory, error) {
	const op = errors.Op("network factory init")
	if server.cfg.Relay == "" || server.cfg.Relay == "pipes" {
		return roadrunner.NewPipeFactory(), nil
	}

	dsn := strings.Split(server.cfg.Relay, "://")
	if len(dsn) != 2 {
		return nil, errors.E(op, errors.Network, errors.Str("invalid DSN (tcp://:6001, unix://file.sock)"))
	}

	lsn, err := util.CreateListener(server.cfg.Relay)
	if err != nil {
		return nil, errors.E(op, errors.Network, err)
	}

	switch dsn[0] {
	// sockets group
	case "unix":
		return roadrunner.NewSocketServer(lsn, server.cfg.RelayTimeout), nil
	case "tcp":
		return roadrunner.NewSocketServer(lsn, server.cfg.RelayTimeout), nil
	default:
		return nil, errors.E(op, errors.Network, errors.Str("invalid DSN (tcp://:6001, unix://file.sock)"))
	}
}

func (server *Plugin) setEnv(e server.Env) []string {
	env := append(os.Environ(), fmt.Sprintf("RR_RELAY=%s", server.cfg.Relay))
	for k, v := range e {
		env = append(env, fmt.Sprintf("%s=%s", strings.ToUpper(k), v))
	}

	return env
}

func (server *Plugin) collectLogs(event interface{}) {
	if we, ok := event.(roadrunner.WorkerEvent); ok {
		switch we.Event {
		case roadrunner.EventWorkerError:
			server.log.Error(we.Payload.(error).Error(), "pid", we.Worker.Pid())
		case roadrunner.EventWorkerLog:
			server.log.Debug(strings.TrimRight(string(we.Payload.([]byte)), " \n\t"), "pid", we.Worker.Pid())
		}
	}
}