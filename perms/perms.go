package perms

import "gopkg.in/macaroon-bakery.v2/bakery"

// RequiredPermissions is a map of all pool RPC methods and their required
// macaroon permissions to access poold.
var RequiredPermissions = map[string][]bakery.Op{
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
	"/poolrpc.Trader/AccountModificationFees": {{
		Entity: "account",
		Action: "read",
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
