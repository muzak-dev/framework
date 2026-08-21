// Command example is a runnable Badele application that mirrors FastAPI's
// "Bigger Applications" tutorial.
//
// It composes three independently written routers, applies a guard to the
// whole application and a second one to the admin subtree, loads its settings
// from the environment, and manages a resource through the application
// lifecycle. Run it and open http://localhost:8080/docs:
//
//	go run .
//
// Every route below the root guard needs a token query parameter, so a working
// request looks like:
//
//	curl 'http://localhost:8080/users/me?token=jessica'
package main

import (
	"context"
	"log"
	"net/http"

	"badele"
	"badele-example/internal/admin"
	"badele-example/routers/items"
	"badele-example/routers/users"
)

// Settings is the application's configuration, read from the environment and
// from a .env file if one is present.
type Settings struct {
	// AppName titles the API in the generated documentation.
	AppName string `env:"APP_NAME" default:"Awesome API"`
	// AdminEmail is who to contact about the API.
	AdminEmail string `env:"ADMIN_EMAIL" default:"admin@example.com"`
	// ItemsPerUser caps how many items one user may hold.
	ItemsPerUser int `env:"ITEMS_PER_USER" default:"50"`
	// Addr is the address the server listens on.
	Addr string `env:"ADDR" default:":8080"`
}

// InfoOut is the response model for the settings endpoint. It is a separate
// type from Settings on purpose: the response exposes exactly these three
// fields, and adding a secret to Settings later cannot leak it here.
type InfoOut struct {
	AppName      string `json:"app_name"`
	AdminEmail   string `json:"admin_email"`
	ItemsPerUser int    `json:"items_per_user"`
}

// PredictParams binds the input of the prediction route.
type PredictParams struct {
	// X is the value to run through the model.
	X float64 `query:"x" doc:"The value to predict from" default:"1"`
}

// PredictOut is the response model for the prediction route.
type PredictOut struct {
	Result float64 `json:"result"`
}

func main() {
	settings := badele.MustLoadConfig[Settings](badele.EnvFile(".env"))

	// A plain map managed by the application lifecycle. Start fills it before
	// the server accepts traffic and Stop releases it after the server has
	// drained, which is the Go counterpart of FastAPI's lifespan.
	models := map[string]func(float64) float64{}

	app := badele.New(badele.AppOptions{
		Title:       settings.AppName,
		Version:     "1.0.0",
		Description: "The Bigger Applications example, rebuilt on Badele.",
		Contact:     &badele.Contact{Email: settings.AdminEmail},
		Addr:        settings.Addr,
	},
		badele.WithDependencies(GetQueryToken),
		badele.WithSingleton(settings),
		badele.WithSingleton(models, badele.LifecycleFunc("ml-model",
			func(ctx context.Context) error {
				models["answer_to_everything"] = func(x float64) float64 { return x * 42 }
				return nil
			},
			func(ctx context.Context) error {
				clear(models)
				return nil
			},
		)),
	)

	app.Include(users.NewRouter())
	app.Include(items.NewRouter())
	app.Include(admin.NewRouter(),
		badele.WithPrefix("/admin"),
		badele.WithTags("admin"),
		badele.WithDependencies(GetTokenHeader),
		badele.WithResponseDoc(http.StatusTeapot, "I'm a teapot"),
	)

	// Routes registered on the app itself use the same generic methods any
	// nested router uses, because App embeds *Router.
	app.Get("/info", info,
		badele.WithTags("meta"),
		badele.Summary("Report the running configuration"))

	app.Get("/predict", predict,
		badele.WithTags("meta"),
		badele.Summary("Run the loaded model"))

	app.Get("/healthz", health,
		badele.Hidden(),
		badele.Summary("Liveness probe"))

	if err := app.RunSignals(); err != nil {
		log.Fatal(err)
	}
}

// info reports the loaded settings. The singleton is retrieved by type, with
// no cast.
func info(ctx *badele.Context, _ badele.Empty) (InfoOut, error) {
	s := badele.From[Settings](ctx)
	return InfoOut{
		AppName:      s.AppName,
		AdminEmail:   s.AdminEmail,
		ItemsPerUser: s.ItemsPerUser,
	}, nil
}

// predict runs the model the lifecycle component loaded.
func predict(ctx *badele.Context, in PredictParams) (PredictOut, error) {
	models := badele.From[map[string]func(float64) float64](ctx)
	model, ready := models["answer_to_everything"]
	if !ready {
		return PredictOut{}, badele.NewHTTPError(http.StatusServiceUnavailable, "the model is not loaded")
	}
	return PredictOut{Result: model(in.X)}, nil
}

// HealthOut is the response model for the liveness probe.
type HealthOut struct {
	Status string `json:"status"`
}

// health answers the liveness probe. It is hidden from the documentation but
// still routable.
func health(ctx *badele.Context, _ badele.Empty) (HealthOut, error) {
	return HealthOut{Status: "ok"}, nil
}
