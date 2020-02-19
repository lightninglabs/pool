package auction

// Phase is an enum-like variable that describes the current phase of the
// auction from the PoV of either the server or the client.
type Phase uint32

const (
	// OrderSubmitPhase is the state the client is in once they have an
	// active and valid account. During this phase the client is free to
	// submit new orders and modify any existing orders.
	OrderSubmitPhase Phase = iota

	// BatchClearingPhase is the phase the auction enters once the
	// OrderSubmitPhase has ended, and the auctioneer is able to make a
	// market. During this phase, the auctioneer enters into an interactive
	// protocol with each active trader which is a part of this batch to
	// sign relevant inputs for the funding transaction, and also to carry
	// out the normal funding flow process so they receive valid commitment
	// transactions.
	BatchClearingPhase
)
