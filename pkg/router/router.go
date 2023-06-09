package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/acorn-io/baaah/pkg/backend"
	"github.com/acorn-io/baaah/pkg/leader"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Router struct {
	RouteBuilder

	OnErrorHandler ErrorHandler
	handlers       *HandlerSet
	electionConfig *leader.ElectionConfig
	hasHealthz     bool
}

// New returns a new *Router with given HandlerSet and ElectionConfig. Passing a nil ElectionConfig is valid and results
// in no leader election for the router.
// The healthzPort is the port on which the healthz endpoint will be served. If <= 0, the healthz endpoint will not be
// served. When creating multiple routers, the first router created with a positive healthzPort will be used.
// The healthz endpoint is served on /healthz, and will not be started until the router is started.
func New(handlerSet *HandlerSet, electionConfig *leader.ElectionConfig, healthzPort int) *Router {
	r := &Router{
		handlers:       handlerSet,
		electionConfig: electionConfig,
	}

	if healthzPort > 0 {
		setPort(healthzPort)
		r.hasHealthz = true
	}

	r.RouteBuilder.router = r
	return r
}

func (r *Router) Backend() backend.Backend {
	return r.handlers.backend
}

type RouteBuilder struct {
	includeRemove bool
	finalizeID    string
	router        *Router
	objType       kclient.Object
	name          string
	namespace     string
	routeName     string
	middleware    []Middleware
	sel           labels.Selector
	fieldSelector fields.Selector
}

func (r RouteBuilder) Middleware(m ...Middleware) RouteBuilder {
	r.middleware = append(r.middleware, m...)
	return r
}

func (r RouteBuilder) Namespace(namespace string) RouteBuilder {
	r.namespace = namespace
	return r
}

func (r RouteBuilder) Selector(sel labels.Selector) RouteBuilder {
	r.sel = sel
	return r
}

func (r RouteBuilder) FieldSelector(sel fields.Selector) RouteBuilder {
	r.fieldSelector = sel
	return r
}

func (r RouteBuilder) Name(name string) RouteBuilder {
	r.name = name
	return r
}

func (r RouteBuilder) IncludeRemoved() RouteBuilder {
	r.includeRemove = true
	return r
}

func (r RouteBuilder) Finalize(finalizerID string, h Handler) {
	r.finalizeID = finalizerID
	r.routeName = name()
	r.Handler(h)
}

func name() string {
	_, filename, line, _ := runtime.Caller(2)
	return fmt.Sprintf("%s:%d", filepath.Base(filename), line)
}

func (r RouteBuilder) FinalizeFunc(finalizerID string, h HandlerFunc) {
	r.finalizeID = finalizerID
	r.routeName = name()
	r.Handler(h)
}

func (r RouteBuilder) Type(objType kclient.Object) RouteBuilder {
	r.objType = objType
	return r
}

func (r RouteBuilder) HandlerFunc(h HandlerFunc) {
	r.routeName = name()
	r.Handler(h)
}

func (r RouteBuilder) Handler(h Handler) {
	if r.routeName == "" {
		r.routeName = name()
	}
	result := h
	if r.finalizeID != "" {
		result = FinalizerHandler{
			FinalizerID: r.finalizeID,
			Next:        result,
		}
	}
	for i := len(r.middleware) - 1; i >= 0; i-- {
		result = r.middleware[i](result)
	}
	if r.name != "" || r.namespace != "" {
		result = NameNamespaceFilter{
			Next:      result,
			Name:      r.name,
			Namespace: r.namespace,
		}
	}
	if r.sel != nil {
		result = SelectorFilter{
			Next:     result,
			Selector: r.sel,
		}
	}
	if r.fieldSelector != nil {
		result = FieldSelectorFilter{
			Next:          result,
			FieldSelector: r.fieldSelector,
		}
	}
	if !r.includeRemove && r.finalizeID == "" {
		result = IgnoreRemoveHandler{
			Next: result,
		}
	}

	if r.routeName != "" {
		result = ErrorPrefix{
			prefix: "[" + r.routeName + "] ",
			Next:   result,
		}
	}

	r.router.handlers.AddHandler(r.objType, result)
}

func (r *Router) Start(ctx context.Context) error {
	id, err := os.Hostname()
	if err != nil {
		return err
	}

	if r.hasHealthz {
		startHealthz(ctx)
	}

	r.handlers.onError = r.OnErrorHandler

	// It's OK to start the electionConfig even if it's nil.
	return r.electionConfig.Run(ctx, id, r.startHandlers, func(leader string) {
		// I am not the leader, so I am healthy until my controllers are started.
		if r.hasHealthz {
			setHealthy(r.name, id != leader)
		}
	})
}

// startHandlers gets called when we become the leader or if there is no leader election.
func (r *Router) startHandlers(ctx context.Context) error {
	var err error
	// This is the leader now, so not ready until the controller is started and caches are ready.
	if r.hasHealthz {
		setHealthy(r.name, false)
		defer setHealthy(r.name, err == nil)
	}

	err = r.handlers.Start(ctx)

	return err
}

func (r *Router) Handle(objType kclient.Object, h Handler) {
	r.routeName = name()
	r.RouteBuilder.Type(objType).Handler(h)
}

func (r *Router) HandleFunc(objType kclient.Object, h HandlerFunc) {
	r.routeName = name()
	r.RouteBuilder.Type(objType).Handler(h)
}

type IgnoreRemoveHandler struct {
	Next Handler
}

func (i IgnoreRemoveHandler) Handle(req Request, resp Response) error {
	if req.Object == nil || !req.Object.GetDeletionTimestamp().IsZero() {
		return nil
	}
	return i.Next.Handle(req, resp)
}

type ErrorPrefix struct {
	prefix string
	Next   Handler
}

type errorPrefix struct {
	Prefix string
	Err    error
}

func (e errorPrefix) Error() string {
	return e.Prefix + e.Err.Error()
}

func (e errorPrefix) Unwrap() error {
	return e.Err
}

func (e ErrorPrefix) Handle(req Request, resp Response) error {
	err := e.Next.Handle(req, resp)
	if err == nil {
		return nil
	}
	return errorPrefix{
		Prefix: e.prefix,
		Err:    err,
	}
}

type NameNamespaceFilter struct {
	Next      Handler
	Name      string
	Namespace string
}

func (n NameNamespaceFilter) Handle(req Request, resp Response) error {
	if n.Name != "" && req.Name != n.Name {
		return nil
	}
	if n.Namespace != "" && req.Namespace != n.Namespace {
		return nil
	}
	return n.Next.Handle(req, resp)
}

type SelectorFilter struct {
	Next     Handler
	Selector labels.Selector
}

func (s SelectorFilter) Handle(req Request, resp Response) error {
	if req.Object == nil || !s.Selector.Matches(labels.Set(req.Object.GetLabels())) {
		return nil
	}
	return s.Next.Handle(req, resp)
}

type FieldSelectorFilter struct {
	Next          Handler
	FieldSelector fields.Selector
}

func (s FieldSelectorFilter) Handle(req Request, resp Response) error {
	if req.Object == nil {
		return nil
	}
	if i, ok := req.Object.(fields.Fields); ok && s.FieldSelector.Matches(i) {
		return s.Next.Handle(req, resp)
	}
	return nil
}
