package portalgql

import (
	"net/http"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/looplj/axonhub/internal/server/biz"
)

//go:generate go run ../gqlgen/generate.go

type GraphqlHandler struct{ Graphql http.Handler }

func NewGraphqlHandlers(service *biz.PortalService, users *biz.UserService) *GraphqlHandler {
	srv := handler.New(NewExecutableSchema(Config{Resolvers: &Resolver{Service: service, Users: users}}))
	srv.AddTransport(transport.POST{})
	srv.Use(extension.FixedComplexityLimit(600))
	// No GET, node, upload, subscriptions, playground, raw evidence or management DTOs.
	return &GraphqlHandler{Graphql: srv}
}
