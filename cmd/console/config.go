package main

import (
	kernelconfig "github.com/bakhod1r/payme-mock/internal/kernel/config"
)

// config is everything the console reads from the environment.
//
// The requirements are declared on the fields rather than checked by hand, so
// a missing setting is reported by name before anything starts listening.
type config struct {
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty" desc:"PostgreSQL connection string" secret:"true"`
	HTTPAddr    string `env:"HTTP_ADDR" default:":8080" desc:"listen address for the console"`

	// Username and Password guard the console. The stand holds no real money,
	// but it does hold every sandbox's keys, so the UI is never open. A blank
	// password would leave it open to anyone who can reach the port, which is
	// why the field is notEmpty rather than merely required.
	Username string `env:"CONSOLE_USER" default:"admin" desc:"console login"`
	Password string `env:"CONSOLE_PASSWORD,required,notEmpty" desc:"console password" secret:"true"`

	// OpenOnLoopback lets a request from this machine through without a login,
	// which is what makes the local stand usable without typing a password. It
	// never applies to a request arriving over the network.
	OpenOnLoopback bool `env:"CONSOLE_OPEN_ON_LOOPBACK" default:"true" desc:"skip the login for requests from this machine"`

	// TrustPrivateNetwork widens that to any private address, which is what a
	// container needs: Docker rewrites the peer to the bridge gateway, so a
	// request typed into a browser on this machine arrives from 172.x and the
	// loopback test never matches.
	//
	// It is only safe where the published port is bound to 127.0.0.1, which is
	// how docker-compose publishes the console. Bound to every interface it
	// would hand every sandbox's keys to anyone on the network, so it is off
	// unless it is asked for by name.
	TrustPrivateNetwork bool `env:"CONSOLE_TRUST_PRIVATE_NET" default:"false" desc:"treat private-network peers as local; only safe when the port is bound to 127.0.0.1"`

	// GatewayBaseURL is the address the console prints in the endpoint column,
	// which is what a merchant pastes into their cash register settings.
	GatewayBaseURL string `env:"GATEWAY_BASE_URL" default:"https://localhost:8443" desc:"base of each sandbox's endpoint URL"`

	// StandBaseURL is where the stand's own APIs answer, which is what the
	// copyable curl on a logged call points at. It is the operator's side of
	// the wire rather than the gateway's: a command pasted into a terminal has
	// to reach the stand from where that terminal is.
	StandBaseURL string `env:"STAND_BASE_URL" default:"http://localhost" desc:"base of the stand's own API addresses, used for the copyable curl of a logged call"`
}

func loadConfig() (config, error) {
	cfg, err := kernelconfig.Load[config]()
	if err != nil {
		return config{}, err
	}
	return *cfg, nil
}
