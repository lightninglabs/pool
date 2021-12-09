// Code generated by MockGen. DO NOT EDIT.
// Source: interfaces.go

// Package pool is a generated GoMock package.
package pool

import (
	context "context"
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
	account "github.com/lightninglabs/pool/account"
	poolrpc "github.com/lightninglabs/pool/poolrpc"
)

// MockMarshaler is a mock of Marshaler interface.
type MockMarshaler struct {
	ctrl     *gomock.Controller
	recorder *MockMarshalerMockRecorder
}

// MockMarshalerMockRecorder is the mock recorder for MockMarshaler.
type MockMarshalerMockRecorder struct {
	mock *MockMarshaler
}

// NewMockMarshaler creates a new mock instance.
func NewMockMarshaler(ctrl *gomock.Controller) *MockMarshaler {
	mock := &MockMarshaler{ctrl: ctrl}
	mock.recorder = &MockMarshalerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMarshaler) EXPECT() *MockMarshalerMockRecorder {
	return m.recorder
}

// MarshallAccountsWithAvailableBalance mocks base method.
func (m *MockMarshaler) MarshallAccountsWithAvailableBalance(ctx context.Context, accounts []*account.Account) ([]*poolrpc.Account, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "MarshallAccountsWithAvailableBalance", ctx, accounts)
	ret0, _ := ret[0].([]*poolrpc.Account)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// MarshallAccountsWithAvailableBalance indicates an expected call of MarshallAccountsWithAvailableBalance.
func (mr *MockMarshalerMockRecorder) MarshallAccountsWithAvailableBalance(ctx, accounts interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "MarshallAccountsWithAvailableBalance", reflect.TypeOf((*MockMarshaler)(nil).MarshallAccountsWithAvailableBalance), ctx, accounts)
}