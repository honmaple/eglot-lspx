package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/sourcegraph/jsonrpc2"
	"go.lsp.dev/protocol"
	"golang.org/x/sync/errgroup"
)

type stdrwc struct{}

func (stdrwc) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (c stdrwc) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (c stdrwc) Close() error {
	return errors.Join(os.Stdin.Close(), os.Stdout.Close())
}

type ProxyServer struct {
	conn      *jsonrpc2.Conn
	procs     []*ProcessServer
	providers map[string][]string
}

func handleProcs[T any](ctx context.Context, req *jsonrpc2.Request, procs []*ProcessServer) ([]T, error) {
	eg := errgroup.Group{}

	results := make([]T, len(procs))
	for i, proc := range procs {
		index := i
		newProc := proc

		eg.Go(func() error {
			var result T

			if err := newProc.Call(ctx, req.Method, req.Params, &result); err != nil {
				return err
			}
			results[index] = result
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *ProxyServer) Wait() error {
	<-s.conn.DisconnectNotify()
	return nil
}

func (s *ProxyServer) Close() error {
	return s.conn.Close()
}

func (s *ProxyServer) getProcsByProviders(method string) []*ProcessServer {
	providerMap := map[string]string{
		protocol.MethodTextDocumentHover:              "hover",
		protocol.MethodTextDocumentCompletion:         "completion",
		protocol.MethodTextDocumentDefinition:         "definition",
		protocol.MethodTextDocumentRename:             "rename",
		protocol.MethodTextDocumentReferences:         "references",
		protocol.MethodTextDocumentPublishDiagnostics: "diagnostic",
	}

	m, ok := providerMap[method]
	if !ok {
		return s.procs
	}
	providers, ok := s.providers[m]
	if !ok || len(providers) == 0 {
		return s.procs
	}

	procs := make([]*ProcessServer, 0)
	for i, proc := range s.procs {
		if slices.Contains(providers, fmt.Sprintf("%d", i)) || slices.Contains(providers, proc.Name()) {
			procs = append(procs, proc)
		}
	}
	if len(procs) == 0 {
		return s.procs
	}
	return procs
}

func (s *ProxyServer) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	procs := s.getProcsByProviders(req.Method)

	if req.Notif {
		if err := s.handleNotify(ctx, req, procs); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}

	result, err := s.handle(ctx, req, procs)
	if err != nil {
		if err := conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInternalError,
			Message: err.Error(),
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}
	if err := conn.Reply(ctx, req.ID, result); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (s *ProxyServer) handleNotify(ctx context.Context, req *jsonrpc2.Request, procs []*ProcessServer) error {
	eg := errgroup.Group{}

	for _, proc := range procs {
		newProc := proc

		eg.Go(func() error {
			return newProc.Notify(ctx, req.Method, req.Params)
		})
	}
	return eg.Wait()
}

func (s *ProxyServer) handle(ctx context.Context, req *jsonrpc2.Request, procs []*ProcessServer) (any, error) {
	switch req.Method {
	case protocol.MethodInitialize:
		return s.handleInitialize(ctx, req, s.procs)
	case protocol.MethodTextDocumentCompletion:
		return s.handleTextDocumentCompletion(ctx, req, procs)
	default:
		results, err := handleProcs[json.RawMessage](ctx, req, procs)
		if err != nil {
			return nil, err
		}
		return results[0], nil
	}
}

func (s *ProxyServer) handleInitialize(ctx context.Context, req *jsonrpc2.Request, procs []*ProcessServer) (any, error) {
	results, err := handleProcs[protocol.InitializeResult](ctx, req, procs)
	if err != nil {
		return nil, err
	}

	inititalize := protocol.InitializeResult{
		ServerInfo:   results[0].ServerInfo,
		Capabilities: results[0].Capabilities,
	}

	for _, result := range results[1:] {
		newCapabilities := merge(&inititalize.Capabilities, &result.Capabilities)

		c, ok := newCapabilities.(*protocol.ServerCapabilities)
		if ok {
			inititalize.Capabilities = *c
		}
	}
	return inititalize, nil
}

func (s *ProxyServer) handleTextDocumentCompletion(ctx context.Context, req *jsonrpc2.Request, procs []*ProcessServer) (any, error) {
	results, err := handleProcs[protocol.CompletionList](ctx, req, procs)
	if err != nil {
		return nil, err
	}

	completion := protocol.CompletionList{
		Items:        make([]protocol.CompletionItem, 0),
		IsIncomplete: false,
	}

	for _, result := range results {
		if result.IsIncomplete {
			completion.IsIncomplete = true
			continue
		}
		completion.Items = slices.Grow(completion.Items, len(completion.Items)+len(result.Items))
		completion.Items = append(completion.Items, result.Items...)
	}
	return completion, nil
}

func NewProxyServer(ctx context.Context, procs []*ProcessServer, providers map[string][]string) (*ProxyServer, error) {
	proxy := &ProxyServer{
		procs:     procs,
		providers: providers,
	}
	proxy.conn = jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(stdrwc{}, VSCodeObjectCodec{}), proxy)
	for _, proc := range procs {
		proc.proxyConn = proxy.conn
	}
	return proxy, nil
}
