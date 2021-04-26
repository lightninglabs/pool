package pool

import (
	"context"

	"google.golang.org/grpc"
)

func (s *Server) startMacaroonService() error {
	return nil
}

func (s *Server) stopMacaroonService() error {
	return nil
}

// macaroonInterceptor creates macaroon security interceptors.
func (s *Server) macaroonInterceptor() (grpc.UnaryServerInterceptor,
	grpc.StreamServerInterceptor, error) {

	return noopUnaryInterceptor, noopStreamInterceptor, nil
}

func noopUnaryInterceptor(ctx context.Context, req interface{},
	_ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (interface{}, error) {

	return h(ctx, req)
}

func noopStreamInterceptor(srv interface{}, ss grpc.ServerStream,
	_ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

	return handler(srv, ss)
}
