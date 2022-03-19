package router

import (
	"context"

	"github.com/ibuildthecloud/baaah/pkg/meta"
	"k8s.io/apimachinery/pkg/labels"
)

type Router struct {
	RouteBuilder

	handlers *HandlerSet
}

func New(handlerSet *HandlerSet) *Router {
	r := &Router{
		handlers: handlerSet,
	}
	r.RouteBuilder.router = r
	return r
}

type RouteBuilder struct {
	includeRemove bool
	router        *Router
	objType       meta.Object
	name          string
	namespace     string
	sel           map[string]string
}

func (r RouteBuilder) Namespace(namespace string) RouteBuilder {
	r.namespace = namespace
	return r
}

func (r RouteBuilder) Selector(sel map[string]string) RouteBuilder {
	r.sel = sel
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

func (r RouteBuilder) Type(objType meta.Object) RouteBuilder {
	r.objType = objType
	return r
}

func (r RouteBuilder) HandlerFunc(h HandlerFunc) {
	r.Handler(h)
}

func (r RouteBuilder) Handler(h Handler) {
	result := h
	if !r.includeRemove {
		result = IgnoreRemoveHandler{
			Next: result,
		}
	}
	if r.name != "" || r.namespace != "" {
		result = NameNamespaceFilter{
			Next:      result,
			Name:      r.name,
			Namespace: r.namespace,
		}
	}
	if len(r.sel) > 0 {
		result = SelectorFilter{
			Next:     result,
			Selector: labels.SelectorFromSet(r.sel),
		}
	}
	r.router.handlers.AddHandler(r.objType, result)
}

func (r *Router) Start(ctx context.Context) error {
	return r.handlers.Start(ctx)
}

func (r *Router) Handle(objType meta.Object, h Handler) {
	r.RouteBuilder.Type(objType).Handler(h)
}

func (r *Router) HandleFunc(objType meta.Object, h HandlerFunc) {
	r.RouteBuilder.Type(objType).Handler(h)
}

type IgnoreRemoveHandler struct {
	Next Handler
}

func (i IgnoreRemoveHandler) Handle(req Request, resp Response) error {
	if req.Object == nil {
		return nil
	}
	return i.Next.Handle(req, resp)
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
