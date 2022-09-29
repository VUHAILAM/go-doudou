package ddhttp

import (
	"context"
	"fmt"
	"github.com/arl/statsviz"
	"github.com/ascarter/requestid"
	"github.com/common-nighthawk/go-figure"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/klauspost/compress/gzhttp"
	"github.com/olekukonko/tablewriter"
	"github.com/rs/cors"
	configui "github.com/unionj-cloud/go-doudou/framework/http/config"
	"github.com/unionj-cloud/go-doudou/framework/http/httprouter"
	"github.com/unionj-cloud/go-doudou/framework/http/model"
	"github.com/unionj-cloud/go-doudou/framework/http/onlinedoc"
	"github.com/unionj-cloud/go-doudou/framework/http/prometheus"
	"github.com/unionj-cloud/go-doudou/framework/http/registry"
	"github.com/unionj-cloud/go-doudou/framework/internal/config"
	"github.com/unionj-cloud/go-doudou/framework/logger"
	"github.com/unionj-cloud/go-doudou/toolkit/cast"
	"github.com/unionj-cloud/go-doudou/toolkit/stringutils"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"
)

// HttpRouterSrv wraps httpRouter router
type HttpRouterSrv struct {
	Router     *httprouter.RouteGroup
	rootRouter *httprouter.Router
	common
}

// NewHttpRouterSrv create a HttpRouterSrv instance
func NewHttpRouterSrv() *HttpRouterSrv {
	rr := config.DefaultGddRouteRootPath
	if stringutils.IsNotEmpty(config.GddRouteRootPath.Load()) {
		rr = config.GddRouteRootPath.Load()
	}
	if stringutils.IsEmpty(rr) {
		rr = "/"
	}
	rootRouter := httprouter.New()
	srv := &HttpRouterSrv{
		Router:     rootRouter.NewGroup(rr),
		rootRouter: rootRouter,
	}
	srv.Middlewares = append(srv.Middlewares,
		tracing,
		metrics,
	)
	if cast.ToBoolOrDefault(config.GddEnableResponseGzip.Load(), config.DefaultGddEnableResponseGzip) {
		gzipMiddleware, err := gzhttp.NewWrapper(gzhttp.ContentTypes(contentTypeShouldbeGzip))
		if err != nil {
			panic(err)
		}
		srv.Middlewares = append(srv.Middlewares, toMiddlewareFunc(gzipMiddleware))
	}
	if cast.ToBoolOrDefault(config.GddLogReqEnable.Load(), config.DefaultGddLogReqEnable) {
		srv.Middlewares = append(srv.Middlewares, log)
	}
	srv.Middlewares = append(srv.Middlewares,
		requestid.RequestIDHandler,
		handlers.ProxyHeaders,
	)
	appType := config.GddAppType.LoadOrDefault(config.DefaultGddAppType)
	switch strings.TrimSpace(appType) {
	case "rest":
		srv.Middlewares = append(srv.Middlewares, rest)
	}
	return srv
}

// AddRoute adds routes to router
func (srv *HttpRouterSrv) AddRoute(route ...model.Route) {
	srv.bizRoutes = append(srv.bizRoutes, route...)
}

func (srv *HttpRouterSrv) printRoutes() {
	logger.Infoln("================ Registered Routes ================")
	var data [][]string
	rr := config.DefaultGddRouteRootPath
	if stringutils.IsNotEmpty(config.GddRouteRootPath.Load()) {
		rr = config.GddRouteRootPath.Load()
	}
	var all []model.Route
	all = append(all, srv.bizRoutes...)
	all = append(all, srv.gddRoutes...)
	all = append(all, srv.debugRoutes...)
	for _, r := range all {
		if strings.HasPrefix(r.Pattern, gddPathPrefix) || strings.HasPrefix(r.Pattern, debugPathPrefix) {
			data = append(data, []string{r.Name, r.Method, r.Pattern})
		} else {
			data = append(data, []string{r.Name, r.Method, path.Clean(rr + r.Pattern)})
		}
	}

	tableString := &strings.Builder{}
	table := tablewriter.NewWriter(tableString)
	table.SetHeader([]string{"Name", "Method", "Pattern"})
	for _, v := range data {
		table.Append(v)
	}
	table.Render() // Send output
	rows := strings.Split(strings.TrimSpace(tableString.String()), "\n")
	for _, row := range rows {
		logger.Infoln(row)
	}
	logger.Infoln("===================================================")
}

// AddMiddleware adds middlewares to the end of chain
func (srv *HttpRouterSrv) AddMiddleware(mwf ...func(http.Handler) http.Handler) {
	for _, item := range mwf {
		srv.Middlewares = append(srv.Middlewares, item)
	}
}

// PreMiddleware adds middlewares to the head of chain
func (srv *HttpRouterSrv) PreMiddleware(mwf ...func(http.Handler) http.Handler) {
	var middlewares []mux.MiddlewareFunc
	for _, item := range mwf {
		middlewares = append(middlewares, item)
	}
	srv.Middlewares = append(middlewares, srv.Middlewares...)
}

func (srv *HttpRouterSrv) newHttpServer() *http.Server {
	write, err := time.ParseDuration(config.GddWriteTimeout.Load())
	if err != nil {
		logger.Debugf("Parse %s %s as time.Duration failed: %s, use default %s instead.\n", string(config.GddWriteTimeout),
			config.GddWriteTimeout.Load(), err.Error(), config.DefaultGddWriteTimeout)
		write, _ = time.ParseDuration(config.DefaultGddWriteTimeout)
	}

	read, err := time.ParseDuration(config.GddReadTimeout.Load())
	if err != nil {
		logger.Debugf("Parse %s %s as time.Duration failed: %s, use default %s instead.\n", string(config.GddReadTimeout),
			config.GddReadTimeout.Load(), err.Error(), config.DefaultGddReadTimeout)
		read, _ = time.ParseDuration(config.DefaultGddReadTimeout)
	}

	idle, err := time.ParseDuration(config.GddIdleTimeout.Load())
	if err != nil {
		logger.Debugf("Parse %s %s as time.Duration failed: %s, use default %s instead.\n", string(config.GddIdleTimeout),
			config.GddIdleTimeout.Load(), err.Error(), config.DefaultGddIdleTimeout)
		idle, _ = time.ParseDuration(config.DefaultGddIdleTimeout)
	}

	httpPort := strconv.Itoa(config.DefaultGddPort)
	if _, err = cast.ToIntE(config.GddPort.Load()); err == nil {
		httpPort = config.GddPort.Load()
	}
	httpHost := config.DefaultGddHost
	if stringutils.IsNotEmpty(config.GddHost.Load()) {
		httpHost = config.GddHost.Load()
	}
	httpServer := &http.Server{
		Addr: strings.Join([]string{httpHost, httpPort}, ":"),
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: write,
		ReadTimeout:  read,
		IdleTimeout:  idle,
		Handler:      srv.rootRouter, // Pass our instance of gorilla/mux in.
	}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		logger.Infof("Http server is listening on %s\n", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil {
			logger.Println(err)
		}
	}()

	return httpServer
}

// Run runs http server
func (srv *HttpRouterSrv) Run() {
	manage := cast.ToBoolOrDefault(config.GddManage.Load(), config.DefaultGddManage)
	if manage {
		srv.Middlewares = append([]mux.MiddlewareFunc{prometheus.PrometheusMiddleware}, srv.Middlewares...)
		gddRouter := srv.rootRouter.NewGroup(gddPathPrefix)
		corsOpts := cors.New(cors.Options{
			AllowedMethods: []string{
				http.MethodGet,
				http.MethodPost,
				http.MethodPut,
				http.MethodPatch,
				http.MethodDelete,
				http.MethodOptions,
				http.MethodHead,
			},

			AllowedHeaders: []string{
				"*",
			},

			AllowOriginRequestFunc: func(r *http.Request, origin string) bool {
				if r.URL.Path == fmt.Sprintf("%sopenapi.json", gddPathPrefix) {
					return true
				}
				return false
			},
		})
		basicAuthMiddle := mux.MiddlewareFunc(basicAuth())
		gddMiddlewares := []mux.MiddlewareFunc{metrics, corsOpts.Handler, basicAuthMiddle}
		srv.gddRoutes = append(srv.gddRoutes, onlinedoc.Routes()...)
		srv.gddRoutes = append(srv.gddRoutes, prometheus.Routes()...)
		srv.gddRoutes = append(srv.gddRoutes, registry.Routes()...)
		srv.gddRoutes = append(srv.gddRoutes, configui.Routes()...)
		freq, err := time.ParseDuration(config.GddStatsFreq.Load())
		if err != nil {
			logger.Debugf("Parse %s %s as time.Duration failed: %s, use default %s instead.\n", string(config.GddStatsFreq),
				config.GddStatsFreq.Load(), err.Error(), config.DefaultGddStatsFreq)
			freq, _ = time.ParseDuration(config.DefaultGddStatsFreq)
		}
		_ = freq
		srv.gddRoutes = append(srv.gddRoutes, []model.Route{
			{
				Name:    "GetStatsvizWs",
				Method:  http.MethodGet,
				Pattern: gddPathPrefix + "statsviz/ws",
			},
			{
				Name:    "GetStatsviz",
				Method:  http.MethodGet,
				Pattern: gddPathPrefix + "statsviz/*filepath",
				HandlerFunc: func(writer http.ResponseWriter, request *http.Request) {
					params := httprouter.ParamsFromContext(request.Context())
					if params.ByName("filepath") == "/ws" {
						statsviz.Ws(writer, request)
						return
					}
					statsviz.IndexAtRoot(gddPathPrefix+"statsviz/").ServeHTTP(writer, request)
				},
			},
		}...)
		for _, item := range srv.gddRoutes {
			if item.HandlerFunc == nil {
				continue
			}
			h := http.Handler(item.HandlerFunc)
			for i := len(gddMiddlewares) - 1; i >= 0; i-- {
				h = gddMiddlewares[i].Middleware(h)
			}
			gddRouter.Handler(item.Method, "/"+strings.TrimPrefix(item.Pattern, gddPathPrefix), h, item.Name)
		}
		srv.debugRoutes = append(srv.debugRoutes, []model.Route{
			{
				Name:    "GetDebugPprofCmdline",
				Method:  http.MethodGet,
				Pattern: debugPathPrefix + "pprof/cmdline",
			},
			{
				Name:    "GetDebugPprofProfile",
				Method:  http.MethodGet,
				Pattern: debugPathPrefix + "pprof/profile",
			},
			{
				Name:    "GetDebugPprofSymbol",
				Method:  http.MethodGet,
				Pattern: debugPathPrefix + "pprof/symbol",
			},
			{
				Name:    "GetDebugPprofTrace",
				Method:  http.MethodGet,
				Pattern: debugPathPrefix + "pprof/trace",
			},
			{
				Name:    "GetDebugPprofIndex",
				Method:  http.MethodGet,
				Pattern: debugPathPrefix + "pprof/*filepath",
				HandlerFunc: func(writer http.ResponseWriter, request *http.Request) {
					params := httprouter.ParamsFromContext(request.Context())
					switch params.ByName("filepath") {
					case "/cmdline":
						pprof.Cmdline(writer, request)
						return
					case "/profile":
						pprof.Profile(writer, request)
						return
					case "/symbol":
						pprof.Symbol(writer, request)
						return
					case "/trace":
						pprof.Trace(writer, request)
						return
					}
					pprof.Index(writer, request)
				},
			},
		}...)
		debugRouter := srv.rootRouter.NewGroup(debugPathPrefix)
		for _, item := range srv.debugRoutes {
			if item.HandlerFunc == nil {
				continue
			}
			h := http.Handler(item.HandlerFunc)
			for i := len(gddMiddlewares) - 1; i >= 0; i-- {
				h = gddMiddlewares[i].Middleware(h)
			}
			debugRouter.Handler(item.Method, "/"+strings.TrimPrefix(item.Pattern, debugPathPrefix), h, item.Name)
		}
	}
	srv.Middlewares = append(srv.Middlewares, recovery)
	for _, item := range srv.bizRoutes {
		h := http.Handler(item.HandlerFunc)
		for i := len(srv.Middlewares) - 1; i >= 0; i-- {
			h = srv.Middlewares[i].Middleware(h)
		}
		srv.Router.Handler(item.Method, item.Pattern, h, item.Name)
	}
	srv.rootRouter.NotFound = http.HandlerFunc(http.NotFound)
	srv.rootRouter.MethodNotAllowed = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("405 method not allowed"))
	})
	for i := len(srv.Middlewares) - 1; i >= 0; i-- {
		srv.rootRouter.NotFound = srv.Middlewares[i].Middleware(srv.rootRouter.NotFound)
		srv.rootRouter.MethodNotAllowed = srv.Middlewares[i].Middleware(srv.rootRouter.MethodNotAllowed)
	}

	start := time.Now()
	banner := config.DefaultGddBanner
	if b, err := cast.ToBoolE(config.GddBanner.Load()); err == nil {
		banner = b
	}
	if banner {
		bannerText := config.DefaultGddBannerText
		if stringutils.IsNotEmpty(config.GddBannerText.Load()) {
			bannerText = config.GddBannerText.Load()
		}
		figure.NewColorFigure(bannerText, "doom", "green", true).Print()
	}

	srv.printRoutes()
	httpServer := srv.newHttpServer()
	defer func() {
		logger.Infoln("http server is shutting down...")

		// Create a deadline to wait for.
		grace, err := time.ParseDuration(config.GddGraceTimeout.Load())
		if err != nil {
			logger.Debugf("Parse %s %s as time.Duration failed: %s, use default %s instead.\n", string(config.GddGraceTimeout),
				config.GddGraceTimeout.Load(), err.Error(), config.DefaultGddGraceTimeout)
			grace, _ = time.ParseDuration(config.DefaultGddGraceTimeout)
		}

		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		// Doesn't block if no connections, but will otherwise wait
		// until the timeout deadline.
		httpServer.Shutdown(ctx)
	}()

	logger.Infof("Started in %s\n", time.Since(start))

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c
}