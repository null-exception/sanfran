package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dosco/sanfran/sidecar/rpc"
	"github.com/golang/glog"
	context "golang.org/x/net/context"
	grpc "google.golang.org/grpc"
)

type server struct {
	lastReqTS  time.Time
	lastPingTS time.Time
	terminate  bool
	mux        sync.Mutex
}

const (
	orphanAfterMin = 10
	appURLPrefix   = "http://localhost:8081"
	funcPath       = "/shared/func"
)

func initServer(port int) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port)) // RPC port
	if err != nil {
		glog.Fatalf("failed to listen: %v", err)
	}
	g := grpc.NewServer()

	t := time.Now()
	server := &server{lastReqTS: t, lastPingTS: t}

	rpc.RegisterSidecarServer(g, server)

	glog.Infof("SanFran/Sidecar Service Listening on :%d\n", port)
	g.Serve(lis)
}

func (s *server) Activate(ctx context.Context, req *rpc.ActivateReq) (*rpc.ActivateResp, error) {
	var err error

	if s.terminate {
		return nil, fmt.Errorf("terminate = true")
	}

	if err := resetFuncFolder(funcPath); err != nil {
		s.terminate = true
		return nil, err
	}

	if len(req.GetCode()) != 0 {
		err = activateFromCode(funcPath, req.GetCode())
	} else if len(req.GetLink()) != 0 {
		err = activateFromLink(funcPath, req.GetLink())
	} else {
		err = fmt.Errorf("No 'code' or 'link' specified")
	}

	if err != nil {
		s.terminate = true
		return nil, err
	}

	reqLink := strings.Join([]string{appURLPrefix, "/api/activate"}, "")
	httpResp, err := http.Get(reqLink)
	if err != nil {
		s.terminate = true
		return nil, err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == 500 {
		s.terminate = true

		body, err := ioutil.ReadAll(httpResp.Body)
		if err != nil {
			return nil, err
		}
		return nil, errors.New(string(body))
	}

	s.mux.Lock()
	t := time.Now()
	s.lastReqTS = t
	s.lastPingTS = t
	s.mux.Unlock()

	return &rpc.ActivateResp{}, nil
}

func (s *server) Execute(ctx context.Context, req *rpc.ExecuteReq) (*rpc.ExecuteResp, error) {
	if s.terminate {
		return nil, fmt.Errorf("terminate = true")
	}

	var url string
	if len(req.GetPath()) == 0 {
		url = strings.Join([]string{appURLPrefix, "/"}, "")
	} else {
		url = strings.Join([]string{appURLPrefix, req.GetPath()}, "/")
	}

	httpReq, err := http.NewRequest(req.Method, url, bytes.NewReader(req.GetBody()))
	if err != nil {
		s.terminate = true
		return nil, err
	}

	httpReq.URL.RawQuery = queryString(req)

	httpReq.Header = http.Header{}
	hdrs := req.GetHeader()
	for k, v := range hdrs {
		httpReq.Header[k] = v.Value
	}

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		s.terminate = true
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		s.terminate = true
		return nil, err
	}

	resp := rpc.ExecuteResp{
		StatusCode: int32(httpResp.StatusCode),
		Status:     httpResp.Status,
		Body:       body,
	}

	resp.Header = make(map[string]*rpc.ListOfString)
	for k, v := range httpResp.Header {
		resp.Header[k] = &rpc.ListOfString{Value: v}
	}

	s.mux.Lock()
	s.lastReqTS = time.Now()
	s.mux.Unlock()

	return &resp, nil
}

type metrics struct {
	LoadAvg []float32 `json:"load_avg"`
	FreeMem float32   `json:"free_mem"`
}

func (s *server) Metrics(ctx context.Context, req *rpc.MetricsReq) (*rpc.MetricsResp, error) {
	if s.terminate {
		return &rpc.MetricsResp{Terminate: true}, nil
	}

	reqLink := strings.Join([]string{appURLPrefix, "/api/ping"}, "")

	httpResp, err := http.Get(reqLink)
	if err != nil {
		s.terminate = true
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		s.terminate = true
		return nil, err
	}

	var m metrics
	if err := json.Unmarshal(body, &m); err != nil {
		s.terminate = true
		return nil, err
	}

	s.mux.Lock()
	t := time.Now()
	resp := &rpc.MetricsResp{
		LoadAvg:   m.LoadAvg,
		FreeMem:   m.FreeMem,
		LastReq:   t.Sub(s.lastReqTS).Seconds(),
		LastPing:  t.Sub(s.lastPingTS).Seconds(),
		Terminate: false,
	}
	if req.GetFromController() {
		s.lastPingTS = t
	}
	s.mux.Unlock()

	return resp, nil
}

func queryString(req *rpc.ExecuteReq) string {
	q := url.Values{}

	for k, v := range req.GetQuery() {
		for i := range v.Value {
			q.Add(k, v.Value[i])
		}
	}

	if len(q) != 0 {
		return q.Encode()
	}
	return ""
}
