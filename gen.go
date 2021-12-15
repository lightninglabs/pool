package pool

// This program generates mocks. It can be invoked by running:
//
// make mock
//

//go:generate mockgen -source=interfaces.go -package=pool -destination=mock_interfaces.go

//go:generate mockgen -source=sidecar/interfaces.go -package=sidecar -destination=sidecar/mock_interfaces.go

//go:generate mockgen -source=internal/test/interfaces.go -package=test -destination=internal/test/mock_interfaces.go

//go:generate mockgen -source=account/interfaces.go -package=account -destination=account/mock_interfaces.go
//go:generate mockgen -source=account/watcher/interfaces.go -package=watcher -destination=account/watcher/mock_interface_test.go

//go:generate mockgen -source=order/interfaces.go -package=order -destination=order/mock_interfaces.go
