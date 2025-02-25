// Copyright (c) 2020 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/registry-facade/api"
	"github.com/gitpod-io/gitpod/registry-facade/pkg/handover"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/remotes"
	"github.com/docker/distribution"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/api/errcode"
	distv2 "github.com/docker/distribution/registry/api/v2"
	"github.com/gorilla/mux"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config configures the registry
type Config struct {
	Port        int    `json:"port"`
	Prefix      string `json:"prefix"`
	StaticLayer []struct {
		Ref  string `json:"ref"`
		Type string `json:"type"`
	} `json:"staticLayer"`
	RemoteSpecProvider *struct {
		Addr string `json:"addr"`
		TLS  *struct {
			Authority   string `json:"ca"`
			Certificate string `json:"crt"`
			PrivateKey  string `json:"key"`
		} `json:"tls,omitempty"`
	} `json:"remoteSpecProvider,omitempty"`
	Store       string `json:"store"`
	RequireAuth bool   `json:"requireAuth"`
	TLS         *struct {
		Certificate string `json:"crt"`
		PrivateKey  string `json:"key"`
	} `json:"tls"`
	Handover struct {
		Enabled bool   `json:"enabled"`
		Sockets string `json:"sockets"`
	} `json:"handover"`
}

// ResolverProvider provides new resolver
type ResolverProvider func() remotes.Resolver

// Registry acts as registry facade
type Registry struct {
	Config         Config
	Resolver       ResolverProvider
	Store          content.Store
	LayerSource    LayerSource
	ConfigModifier ConfigModifier
	SpecProvider   map[string]ImageSpecProvider

	metrics *metrics
	srv     *http.Server
}

// NewRegistry creates a new registry
func NewRegistry(cfg Config, newResolver ResolverProvider, reg prometheus.Registerer) (*Registry, error) {
	storePath := cfg.Store
	if tproot := os.Getenv("TELEPRESENCE_ROOT"); tproot != "" {
		storePath = filepath.Join(tproot, storePath)
	}
	store, err := local.NewStore(storePath)
	if err != nil {
		return nil, err
	}
	// TODO: GC the store

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics, err := newMetrics(reg, true)
	if err != nil {
		return nil, err
	}

	var layerSources []LayerSource

	ideRefSource := func(s *api.ImageSpec) (ref string, err error) {
		return s.IdeRef, nil
	}
	ideLayerSource, err := NewSpecMappedImageSource(newResolver, ideRefSource)
	if err != nil {
		return nil, err
	}
	layerSources = append(layerSources, ideLayerSource)

	log.Info("preparing static layer")
	for _, sl := range cfg.StaticLayer {
		switch sl.Type {
		case "file":
			src, err := NewFileLayerSource(ctx, sl.Ref)
			if err != nil {
				return nil, fmt.Errorf("cannot source layer from %s: %w", sl.Ref, err)
			}
			layerSources = append(layerSources, src)
		case "image":
			src, err := NewStaticSourceFromImage(ctx, newResolver(), sl.Ref)
			if err != nil {
				return nil, fmt.Errorf("cannot source layer from %s: %w", sl.Ref, err)
			}
			layerSources = append(layerSources, src)
		default:
			return nil, fmt.Errorf("unknown static layer type: %s", sl.Type)
		}
	}
	clsrc, err := NewContentLayerSource()
	if err != nil {
		return nil, xerrors.Errorf("cannot create content layer source: %w", err)
	}
	layerSources = append(layerSources, clsrc)

	specProvider := map[string]ImageSpecProvider{}
	if cfg.RemoteSpecProvider != nil {
		opts := []grpc.DialOption{
			grpc.WithUnaryInterceptor(grpc_opentracing.UnaryClientInterceptor(grpc_opentracing.WithTracer(opentracing.GlobalTracer()))),
			grpc.WithStreamInterceptor(grpc_opentracing.StreamClientInterceptor(grpc_opentracing.WithTracer(opentracing.GlobalTracer()))),
		}

		if cfg.RemoteSpecProvider.TLS != nil {
			ca := cfg.RemoteSpecProvider.TLS.Authority
			crt := cfg.RemoteSpecProvider.TLS.Certificate
			key := cfg.RemoteSpecProvider.TLS.PrivateKey

			// Telepresence (used for debugging only) requires special paths to load files from
			if root := os.Getenv("TELEPRESENCE_ROOT"); root != "" {
				ca = filepath.Join(root, ca)
				crt = filepath.Join(root, crt)
				key = filepath.Join(root, key)
			}

			rootCA, err := os.ReadFile(ca)
			if err != nil {
				return nil, xerrors.Errorf("could not read ca certificate: %s", err)
			}
			certPool := x509.NewCertPool()
			if ok := certPool.AppendCertsFromPEM(rootCA); !ok {
				return nil, xerrors.Errorf("failed to append ca certs")
			}

			certificate, err := tls.LoadX509KeyPair(crt, key)
			if err != nil {
				log.WithField("config", cfg.TLS).Error("Cannot load ws-manager certs - this is a configuration issue.")
				return nil, xerrors.Errorf("cannot load ws-manager certs: %w", err)
			}

			creds := credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{certificate},
				RootCAs:      certPool,
			})
			opts = append(opts, grpc.WithTransportCredentials(creds))
			log.
				WithField("ca", ca).
				WithField("cert", crt).
				WithField("key", key).
				Debug("using TLS config to connect ws-manager")
		} else {
			opts = append(opts, grpc.WithInsecure())
		}

		specprov, err := NewCachingSpecProvider(128, NewRemoteSpecProvider(cfg.RemoteSpecProvider.Addr, opts))
		if err != nil {
			return nil, xerrors.Errorf("cannot create caching spec provider: %w", err)
		}
		specProvider[api.ProviderPrefixRemote] = specprov
	}

	layerSource := CompositeLayerSource(layerSources)
	return &Registry{
		Config:         cfg,
		Resolver:       newResolver,
		Store:          store,
		SpecProvider:   specProvider,
		LayerSource:    layerSource,
		ConfigModifier: NewConfigModifierFromLayerSource(layerSource),
		metrics:        metrics,
	}, nil
}

// Serve serves the registry on the given port
func (reg *Registry) Serve() error {
	routes := distv2.RouterWithPrefix(reg.Config.Prefix)
	reg.registerHandler(routes)

	var handler http.Handler = routes
	if reg.Config.RequireAuth {
		handler = reg.requireAuthentication(routes)
	}
	mux := http.NewServeMux()
	mux.Handle("/", handler)

	if addr := os.Getenv("REGFAC_NO_TLS_DEBUG"); addr != "" {
		// Gitpod port-forwarding also does SSL termination. If we only served the HTTPS service
		// when using telepresence we could not make any requests to the registry facade directly,
		// e.g. using curl or another Docker daemon. Using the env var we can enable an additional
		// HTTP service.
		//
		// Note: this is is just meant for a telepresence setup
		go http.ListenAndServe(addr, mux)
	}

	addr := fmt.Sprintf(":%d", reg.Config.Port)
	var (
		l   net.Listener
		err error
	)
	if fn := reg.Config.Handover.Sockets; reg.Config.Handover.Enabled && fn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		l, err = ReceiveHandover(ctx, reg.Config.Handover.Sockets)
		cancel()
		if err != nil {
			log.WithError(err).Warn("handover failed - attempting to start socket directly")
		}
	}
	if l == nil {
		// there was no handover configured or available - start our own listener
		l, err = net.Listen("tcp", addr)
		if err != nil {
			return err
		}
	}

	reg.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	var hoc <-chan bool
	if reg.Config.Handover.Enabled {
		hoctx, cancelHO := context.WithCancel(context.Background())
		defer cancelHO()
		hoc, err = OfferHandover(hoctx, reg.Config.Handover.Sockets, l, reg.srv)
		if err != nil {
			return err
		}
	}

	if reg.Config.TLS != nil {
		log.WithField("addr", addr).Info("HTTPS registry server listening")

		cert, key := reg.Config.TLS.Certificate, reg.Config.TLS.PrivateKey
		if tproot := os.Getenv("TELEPRESENCE_ROOT"); tproot != "" {
			cert = filepath.Join(tproot, cert)
			key = filepath.Join(tproot, key)
		}

		return reg.srv.ServeTLS(l, cert, key)
	}

	srvErrChan := make(chan error, 1)
	go func() {
		log.WithField("addr", addr).Info("HTTP registry server listening")
		srvErrChan <- reg.srv.Serve(l)
	}()

	select {
	case err := <-srvErrChan:
		return err
	case handingOver := <-hoc:
		if !handingOver {
			return nil
		}
		// we are handing over and must wait for the server to shut down
		<-hoc
		return nil
	}
}

// MustServe calls serve and logs any error as Fatal
func (reg *Registry) MustServe() {
	err := reg.Serve()
	if err != nil {
		log.WithError(err).Fatal("cannot serve registry")
	}
}

// ReceiveHandover lists all Unix sockets in loc, finds the latest and attempts a Listener
// handover from that socket. If loc == "", this function returns nil, nil.
func ReceiveHandover(ctx context.Context, loc string) (l net.Listener, err error) {
	if loc == "" {
		return nil, nil
	}
	fs, err := os.ReadDir(loc)
	if err != nil {
		return nil, err
	}
	var fn string
	for _, f := range fs {
		if f.Type()*os.ModeSocket == 0 {
			continue
		}
		if f.Name() > fn {
			fn = f.Name()
		}
	}
	if fn == "" {
		return nil, nil
	}
	fn = filepath.Join(loc, fn)

	log.WithField("fn", fn).Debug("found handover socket - attempting listener handover")
	return handover.ReceiveHandover(ctx, fn)
}

// Shutdowner is a process that can be shut down
type Shutdowner interface {
	Shutdown(context.Context) error
}

// OfferHandover offers the registry-facade listener handover on a Unix socket
func OfferHandover(ctx context.Context, loc string, l net.Listener, s Shutdowner) (handingOver <-chan bool, err error) {
	socketFN := filepath.Join(loc, fmt.Sprintf("rf-handover-%d.sock", time.Now().Unix()))
	if socketFN == "" {
		return nil, nil
	}
	tcpL, ok := l.(*net.TCPListener)
	if !ok {
		return nil, xerrors.Errorf("can only offer handovers for *net.TCPListener")
	}

	handingOverC := make(chan bool)
	go func() {
		defer close(handingOverC)

		err := handover.OfferHandover(ctx, socketFN, tcpL)
		if err != nil {
			log.WithError(err).Error("listener handover offer failed")
			return
		}

		log.Warn("listener handover initiated - not accepting new connections and stopping server")
		handingOverC <- true

		if s == nil {
			return
		}
		err = s.Shutdown(ctx)
		if err != nil {
			log.WithError(err).Warn("error during server shutdown")
			return
		}
		log.Warn("server shutdown complete")
	}()
	log.WithField("socket", socketFN).Info("offering listener handover")
	return handingOverC, nil
}

func (reg *Registry) requireAuthentication(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			w.Header().Add("WWW-Authenticate", "Basic")
			w.WriteHeader(http.StatusUnauthorized)
		}

		_, _, ok := r.BasicAuth()
		if !ok {
			fail()
			return
		}

		// todo: implement auth

		h.ServeHTTP(w, r)
	})
}

// registerHandler registers the handle* functions with the corresponding routes
func (reg *Registry) registerHandler(routes *mux.Router) {
	routes.Get(distv2.RouteNameBase).HandlerFunc(reg.handleAPIBase)
	routes.Get(distv2.RouteNameManifest).Handler(dispatcher(reg.handleManifest))
	// routes.Get(v2.RouteNameCatalog).Handler(dispatcher(reg.handleCatalog))
	// routes.Get(v2.RouteNameTags).Handler(dispatcher(reg.handleTags))
	routes.Get(distv2.RouteNameBlob).Handler(dispatcher(reg.handleBlob))
	// routes.Get(v2.RouteNameBlobUpload).Handler(dispatcher(reg.handleBlobUpload))
	// routes.Get(v2.RouteNameBlobUploadChunk).Handler(dispatcher(reg.handleBlobUploadChunk))
	routes.NotFoundHandler = http.HandlerFunc(reg.handleAPIBase)
}

// handleApiBase implements a simple yes-man for doing overall checks against the
// api. This can support auth roundtrips to support docker login.
func (reg *Registry) handleAPIBase(w http.ResponseWriter, r *http.Request) {
	const emptyJSON = "{}"
	// Provide a simple /v2/ 200 OK response with empty json response.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(emptyJSON)))

	fmt.Fprint(w, emptyJSON)
}

type dispatchFunc func(ctx context.Context, r *http.Request) http.Handler

// dispatcher wraps a dispatchFunc and provides context
func dispatcher(d dispatchFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc, _ := httputil.DumpRequest(r, false)
		fmt.Fprint(os.Stderr, string(fc))

		// Get context from request, add vars and other info and sync back
		ctx := r.Context()
		ctx = &muxVarsContext{
			Context: ctx,
			vars:    mux.Vars(r),
		}
		r = r.WithContext(ctx)

		if nameRequired(r) {
			nameRef, err := reference.WithName(getName(ctx))
			if err != nil {
				log.WithError(err).WithField("nameRef", nameRef).Errorf("error parsing reference from context")
				respondWithError(w, distribution.ErrRepositoryNameInvalid{
					Name:   nameRef.Name(),
					Reason: err,
				})
				return
			}
		}

		d(ctx, r).ServeHTTP(w, r)
	})
}

func respondWithError(w http.ResponseWriter, terr error) {
	err := errcode.ServeJSON(w, terr)
	if err != nil {
		log.WithError(err).WithField("orignalErr", terr).Errorf("error serving error json")
	}
}

// nameRequired returns true if the route requires a name.
func nameRequired(r *http.Request) bool {
	route := mux.CurrentRoute(r)
	if route == nil {
		return true
	}
	routeName := route.GetName()
	return routeName != distv2.RouteNameBase && routeName != distv2.RouteNameCatalog
}

type muxVarsContext struct {
	context.Context
	vars map[string]string
}

func (ctx *muxVarsContext) Value(key interface{}) interface{} {
	if keyStr, ok := key.(string); ok {
		if keyStr == "vars" {
			return ctx.vars
		}

		if strings.HasPrefix(keyStr, "vars.") {
			keyStr = strings.TrimPrefix(keyStr, "vars.")
		}

		if v, ok := ctx.vars[keyStr]; ok {
			return v
		}
	}

	return ctx.Context.Value(key)
}

// getName extracts the name var from the context which was passed in through the mux route
func getName(ctx context.Context) string {
	val := ctx.Value("vars.name")
	sval, ok := val.(string)
	if !ok {
		return ""
	}
	return sval
}

func getSpecProviderName(ctx context.Context) (specProviderName string, remainder string) {
	name := getName(ctx)
	segs := strings.Split(name, "/")
	if len(segs) > 1 {
		specProviderName = segs[0]
		remainder = strings.Join(segs[1:], "/")
	}
	return
}

// getReference extracts the referece var from the context which was passed in through the mux route
func getReference(ctx context.Context) string {
	val := ctx.Value("vars.reference")
	sval, ok := val.(string)
	if !ok {
		return ""
	}
	return sval
}

// getDigest extracts the digest var from the context which was passed in through the mux route
func getDigest(ctx context.Context) string {
	val := ctx.Value("vars.digest")
	sval, ok := val.(string)
	if !ok {
		return ""
	}

	return sval
}
