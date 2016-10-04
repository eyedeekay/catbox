package main

import "fmt"

// Server holds information about a linked server. Local and remote.
type Server struct {
	SID         TS6SID
	Name        string
	Description string
	HopCount    int

	// If this server is directly connected to us (local), then LocalServer is
	// set.
	LocalServer *LocalServer

	// This is the server we heard about the server through.
	// If the server is not directly connected to us (remote), then we know how
	// it is connected to us. Through this LocalServer.
	ClosestServer *LocalServer

	// We know what server it is linked to. The SID message tells us.
	LinkedTo *Server
}

func (s *Server) String() string {
	return fmt.Sprintf("%s %s", s.SID, s.Name)
}

func (s *Server) isLocal() bool {
	return s.LocalServer != nil
}

func (s *Server) isRemote() bool {
	return !s.isLocal()
}

// Find all servers linked to us, directly or not.
func (s *Server) getLinkedServers(allServers map[TS6SID]*Server) []*Server {
	linkedServers := []*Server{}

	for _, server := range allServers {
		if server == s {
			continue
		}

		if server.LinkedTo != s {
			continue
		}

		linkedServers = append(linkedServers,
			server.getLinkedServers(allServers)...)
	}

	return linkedServers
}
