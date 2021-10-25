package poolgen

// This program generates mocks. It can be invoked by running:
//
// make mock
//

//go:generate mockgen -source=../account/watcher/interface.go -package=watcher -destination=../account/watcher/mock_interface_test.go
