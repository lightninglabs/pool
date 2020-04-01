package order

// BatchVersion is the type for the batch verification protocol.
type BatchVersion uint32

const (
	// DefaultVersion is the first implemented version of the batch
	// verification protocol.
	DefaultVersion BatchVersion = 0

	// CurrentVersion must point to the latest implemented version of the
	// batch verification protocol. Both server and client should always
	// refer to this constant. If a client's binary is not updated in time
	// it will point to a previous version than the server and the mismatch
	// will be detected during the OrderMatchPrepare call.
	CurrentVersion = DefaultVersion
)
