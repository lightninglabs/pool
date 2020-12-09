// As this file is very similar in every package, ignore the linter here.
// nolint:dupl,interfacer
package pool

import (
	"context"

	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const Subsystem = "LLMD"

var (
	logWriter = build.NewRotatingLogWriter()
	log       = build.NewSubLogger(Subsystem, logWriter.GenSubLogger)
	rpcLog    = build.NewSubLogger("RPCS", logWriter.GenSubLogger)

	// SupportedSubsystems is a function that returns a list of all
	// supported logging sub systems.
	SupportedSubsystems = logWriter.SupportedSubsystems
)

func init() {
	setSubLogger(Subsystem, log, nil)
	setSubLogger("RPCS", rpcLog, nil)
	addSubLogger(funding.Subsystem, funding.UseLogger)
	addSubLogger(auctioneer.Subsystem, auctioneer.UseLogger)
	addSubLogger(order.Subsystem, order.UseLogger)
	addSubLogger("LNDC", lndclient.UseLogger)
	addSubLogger("SGNL", signal.UseLogger)
	addSubLogger(account.Subsystem, account.UseLogger)
	addSubLogger(lsat.Subsystem, lsat.UseLogger)
	addSubLogger(clientdb.Subsystem, clientdb.UseLogger)
}

// addSubLogger is a helper method to conveniently create and register the
// logger of a sub system.
func addSubLogger(subsystem string, useLogger func(btclog.Logger)) {
	logger := build.NewSubLogger(subsystem, logWriter.GenSubLogger)
	setSubLogger(subsystem, logger, useLogger)
}

// setSubLogger is a helper method to conveniently register the logger of a sub
// system.
func setSubLogger(subsystem string, logger btclog.Logger,
	useLogger func(btclog.Logger)) {

	logWriter.RegisterSubLogger(subsystem, logger)
	if useLogger != nil {
		useLogger(logger)
	}
}

// errorLogUnaryServerInterceptor is a simple UnaryServerInterceptor that will
// automatically log any errors that occur when serving a client's unary
// request.
func errorLogUnaryServerInterceptor(logger btclog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		resp, err := handler(ctx, req)
		if err != nil {
			// TODO(roasbeef): also log request details?
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return resp, err
	}
}

// errorLogUnaryClientInterceptor is a simple UnaryClientInterceptor that will
// automatically log any errors that occur when executing a unary request to
// the server.
func errorLogUnaryClientInterceptor(logger btclog.Logger) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			logger.Errorf("[%v]: %v", method, err)
		}

		return err
	}
}

// errorLogStreamServerInterceptor is a simple StreamServerInterceptor that
// will log any errors that occur while processing a client or server streaming
// RPC.
func errorLogStreamServerInterceptor(logger btclog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		err := handler(srv, ss)
		if err != nil {
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return err
	}
}

// errorLoggingClientStream wraps around the default client stream to long any errors
// that happen when we attempt to send or receive messages on the main stream.
type errorLoggingClientStream struct {
	grpc.ClientStream

	methodName string

	logger btclog.Logger
}

// RecvMsg attempts to recv a message, but logs an error if it occurs.
func (e *errorLoggingClientStream) RecvMsg(m interface{}) error {
	err := e.ClientStream.RecvMsg(m)
	s, ok := status.FromError(err)
	isCtxCanceledErr := ok && s.Code() == codes.Canceled
	if err != nil && !isCtxCanceledErr {
		e.logger.Errorf("[%v]: %v", e.methodName, err)
	}

	return err
}

// SendMsg attempts to send a message, but logs an error if it occurs.
func (e *errorLoggingClientStream) SendMsg(m interface{}) error {
	err := e.ClientStream.SendMsg(m)
	if err != nil {
		e.logger.Errorf("[%v]: %v", e.methodName, err)
	}

	return err
}

// errorLogStreamClientInterceptor is a simple StreamClientInterceptor that
// will log any errors that occur while processing the messages for a server's
// streaming RPC.
func errorLogStreamClientInterceptor(logger btclog.Logger) grpc.StreamClientInterceptor {

	return func(ctx context.Context, desc *grpc.StreamDesc,
		cc *grpc.ClientConn, method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		mainStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			logger.Errorf("[%v]: %v", method, err)
			return nil, err
		}

		return &errorLoggingClientStream{
			ClientStream: mainStream,
			logger:       logger,
			methodName:   method,
		}, nil
	}
}
