package unknowncfg

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type Server struct{ cfg huma.Config }

func (s *Server) New() huma.API {
	return humago.New(http.NewServeMux(), s.cfg) // want "cannot verify"
}
