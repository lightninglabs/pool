package pool

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// poolMacaroonLocation is the value we use for the pool macaroons'
	// "Location" field when baking them.
	poolMacaroonLocation = "pool"

	// macDatabaseOpenTimeout is how long we wait for acquiring the lock on
	// the macaroon database before we give up with an error.
	macDatabaseOpenTimeout = time.Second * 5
)

var (
	// RequiredPermissions is a map of all pool RPC methods and their
	// required macaroon permissions to access poold.
	RequiredPermissions = map[string][]bakery.Op{
		"/poolrpc.Trader/QuoteAccount": {{
			Entity: "account",
			Action: "read",
		}},
		"/poolrpc.Trader/InitAccount": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/ListAccounts": {{
			Entity: "account",
			Action: "read",
		}},
		"/poolrpc.Trader/CloseAccount": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/WithdrawAccount": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/DepositAccount": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/RenewAccount": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/BumpAccountFee": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/RecoverAccounts": {{
			Entity: "account",
			Action: "write",
		}},
		"/poolrpc.Trader/SubmitOrder": {{
			Entity: "order",
			Action: "write",
		}},
		"/poolrpc.Trader/ListOrders": {{
			Entity: "order",
			Action: "read",
		}},
		"/poolrpc.Trader/CancelOrder": {{
			Entity: "order",
			Action: "write",
		}},
		"/poolrpc.Trader/AuctionFee": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/Leases": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/BatchSnapshot": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/GetLsatTokens": {{
			Entity: "auth",
			Action: "read",
		}},
		"/poolrpc.Trader/LeaseDurations": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/NextBatchInfo": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/NodeRatings": {{
			Entity: "auction",
			Action: "read",
		}},
		"/poolrpc.Trader/BatchSnapshots": {{
			Entity: "auction",
			Action: "read",
		}},
	}

	// allPermissions is the list of all existing permissions that exist
	// for poold's RPC. The default macaroon that is created on startup
	// contains all these permissions and is therefore equivalent to lnd's
	// admin.macaroon but for pool.
	allPermissions = []bakery.Op{{
		Entity: "account",
		Action: "read",
	}, {
		Entity: "account",
		Action: "write",
	}, {
		Entity: "order",
		Action: "read",
	}, {
		Entity: "order",
		Action: "write",
	}, {
		Entity: "auction",
		Action: "read",
	}, {
		Entity: "auth",
		Action: "read",
	}}

	// macDbDefaultPw is the default encryption password used to encrypt the
	// pool macaroon database. The macaroon service requires us to set a
	// non-nil password so we set it to an empty string. This will cause the
	// keys to be encrypted on disk but won't provide any security at all as
	// the password is known to anyone.
	//
	// TODO(guggero): Allow the password to be specified by the user. Needs
	// create/unlock calls in the RPC. Using a password should be optional
	// though.
	macDbDefaultPw = []byte("")
)

// startMacaroonService starts the macaroon validation service, creates or
// unlocks the macaroon database and creates the default macaroon if it doesn't
// exist yet. If macaroons are disabled in general in the configuration, none of
// these actions are taken.
func (s *Server) startMacaroonService() error {
	// Create the macaroon authentication/authorization service.
	var err error
	s.macaroonService, err = macaroons.NewService(
		s.cfg.BaseDir, poolMacaroonLocation, false,
		macDatabaseOpenTimeout, macaroons.IPLockChecker,
	)
	if err != nil {
		return fmt.Errorf("unable to set up macaroon authentication: "+
			"%v", err)
	}

	// Try to unlock the macaroon store with the private password.
	err = s.macaroonService.CreateUnlock(&macDbDefaultPw)
	if err != nil {
		return fmt.Errorf("unable to unlock macaroon DB: %v", err)
	}

	// Create macaroon files for pool CLI to use if they don't exist.
	if !lnrpc.FileExists(s.cfg.MacaroonPath) {
		// We don't offer the ability to rotate macaroon root keys yet,
		// so just use the default one since the service expects some
		// value to be set.
		idCtx := macaroons.ContextWithRootKeyID(
			context.Background(), macaroons.DefaultRootKeyID,
		)

		// We only generate one default macaroon that contains all
		// existing permissions (equivalent to the admin.macaroon in
		// lnd). Custom macaroons can be created through the bakery
		// RPC.
		poolMac, err := s.macaroonService.Oven.NewMacaroon(
			idCtx, bakery.LatestVersion, nil, allPermissions...,
		)
		if err != nil {
			return err
		}
		poolMacBytes, err := poolMac.M().MarshalBinary()
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(s.cfg.MacaroonPath, poolMacBytes, 0644)
		if err != nil {
			if err := os.Remove(s.cfg.MacaroonPath); err != nil {
				log.Errorf("Unable to remove %s: %v",
					s.cfg.MacaroonPath, err)
			}
			return err
		}
	}

	return nil
}

// stopMacaroonService closes the macaroon database.
func (s *Server) stopMacaroonService() error {
	return s.macaroonService.Close()
}

// macaroonInterceptor creates macaroon security interceptors.
func (s *Server) macaroonInterceptor() (grpc.UnaryServerInterceptor,
	grpc.StreamServerInterceptor) {

	unaryInterceptor := s.macaroonService.UnaryServerInterceptor(
		RequiredPermissions,
	)
	streamInterceptor := s.macaroonService.StreamServerInterceptor(
		RequiredPermissions,
	)
	return unaryInterceptor, streamInterceptor
}
