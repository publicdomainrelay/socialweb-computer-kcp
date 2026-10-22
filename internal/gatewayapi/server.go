package gatewayapi

import (
	"errors"
	"net/http"
)

type ServerOptions struct {
	Cluster *ClusterAPI

	OAuth *OAuth

	Downloads *Downloads
}

type Server struct {
	handler http.Handler
}

func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Cluster == nil && opts.OAuth == nil && opts.Downloads == nil {
		return nil, errors.New("gatewayapi: server has no routes")
	}
	mux := http.NewServeMux()
	if opts.Cluster != nil {
		opts.Cluster.Register(mux)
	}
	if opts.OAuth != nil {
		opts.OAuth.Register(mux)
	}
	if opts.Downloads != nil {
		opts.Downloads.Register(mux)
	}
	return &Server{handler: mux}, nil
}

func (s *Server) Handler() http.Handler { return s.handler }
