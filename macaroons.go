package pool

import (
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// poolMacaroonLocation is the value we use for the pool macaroons'
	// "Location" field when baking them.
	poolMacaroonLocation = "pool"
)

var (
	// RequiredPermissions is a map of all pool RPC methods and their
	// required macaroon permissions to access poold.
	RequiredPermissions = map[string][]bakery.Op{
		"/poolrpc.Trader/GetInfo": {{
			Entity: "account",
			Action: "read",
		}, {
			Entity: "order",
			Action: "read",
		}, {
			Entity: "auction",
			Action: "read",
		}, {
			Entity: "auth",
			Action: "read",
		}},
		"/poolrpc.Trader/StopDaemon": {{
			Entity: "account",
			Action: "write",
		}},
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
		"/poolrpc.Trader/QuoteOrder": {{
			Entity: "order",
			Action: "read",
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
		"/poolrpc.Trader/OfferSidecar": {{
			Entity: "order",
			Action: "write",
		}},
		"/poolrpc.Trader/RegisterSidecar": {{
			Entity: "order",
			Action: "write",
		}},
		"/poolrpc.Trader/ExpectSidecarChannel": {{
			Entity: "order",
			Action: "write",
		}},
		"/poolrpc.Trader/DecodeSidecarTicket": {{
			Entity: "order",
			Action: "read",
		}},
		"/poolrpc.Trader/ListSidecars": {{
			Entity: "order",
			Action: "read",
		}},
		"/poolrpc.Trader/CancelSidecar": {{
			Entity: "order",
			Action: "write",
		}},
	}

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
