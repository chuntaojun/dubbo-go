/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package getty

import (
	"github.com/apache/dubbo-go/remoting"
	"gopkg.in/yaml.v2"
	"math/rand"
	"time"
)

import (
	"github.com/dubbogo/getty"
	gxsync "github.com/dubbogo/gost/sync"
	perrors "github.com/pkg/errors"
)

import (
	"github.com/apache/dubbo-go/common"
	"github.com/apache/dubbo-go/common/logger"
	"github.com/apache/dubbo-go/config"
)

var (
	errInvalidCodecType  = perrors.New("illegal CodecType")
	errInvalidAddress    = perrors.New("remote address invalid or empty")
	errSessionNotExist   = perrors.New("session not exist")
	errClientClosed      = perrors.New("client closed")
	errClientReadTimeout = perrors.New("client read timeout")

	clientConf   *ClientConfig
	clientGrpool *gxsync.TaskPool
)

func initClient(protocol string) {
	if protocol == "" {
		return
	}

	// load clientconfig from consumer_config
	// default use dubbo
	consumerConfig := config.GetConsumerConfig()
	if consumerConfig.ApplicationConfig == nil {
		return
	}
	protocolConf := config.GetConsumerConfig().ProtocolConf
	defaultClientConfig := GetDefaultClientConfig()
	if protocolConf == nil {
		logger.Info("protocol_conf default use dubbo config")
	} else {
		dubboConf := protocolConf.(map[interface{}]interface{})[protocol]
		if dubboConf == nil {
			logger.Warnf("dubboConf is nil")
			return
		}
		dubboConfByte, err := yaml.Marshal(dubboConf)
		if err != nil {
			panic(err)
		}
		err = yaml.Unmarshal(dubboConfByte, &defaultClientConfig)
		if err != nil {
			panic(err)
		}
	}
	clientConf = &defaultClientConfig
	if err := clientConf.CheckValidity(); err != nil {
		logger.Warnf("[CheckValidity] error: %v", err)
		return
	}
	setClientGrpool()

	rand.Seed(time.Now().UnixNano())
}

// SetClientConf ...
func SetClientConf(c ClientConfig) {
	clientConf = &c
	err := clientConf.CheckValidity()
	if err != nil {
		logger.Warnf("[ClientConfig CheckValidity] error: %v", err)
		return
	}
	setClientGrpool()
}

// GetClientConf ...
func GetClientConf() ClientConfig {
	return *clientConf
}

func setClientGrpool() {
	if clientConf.GrPoolSize > 1 {
		clientGrpool = gxsync.NewTaskPool(gxsync.WithTaskPoolTaskPoolSize(clientConf.GrPoolSize), gxsync.WithTaskPoolTaskQueueLength(clientConf.QueueLen),
			gxsync.WithTaskPoolTaskQueueNumber(clientConf.QueueNumber))
	}
}

// Options ...
type Options struct {
	// connect timeout
	ConnectTimeout time.Duration
	// request timeout
	//RequestTimeout time.Duration
}

//AsyncCallbackResponse async response for dubbo
type AsyncCallbackResponse struct {
	common.CallbackResponse
	Opts      Options
	Cause     error
	Start     time.Time // invoke(call) start time == write start time
	ReadStart time.Time // read start time, write duration = ReadStart - Start
	Reply     interface{}
}

// Client ...
type Client struct {
	addr            string
	opts            Options
	conf            ClientConfig
	pool            *gettyRPCClientPool
	codec           remoting.Codec
	responseHandler remoting.ResponseHandler
	ExchangeClient  *remoting.ExchangeClient
	//sequence atomic.Uint64
	//pendingResponses *sync.Map
}

// NewClient ...
func NewClient(opt Options) *Client {
	switch {
	case opt.ConnectTimeout == 0:
		opt.ConnectTimeout = 3 * time.Second
	}

	c := &Client{
		opts: opt,
	}
	return c
}

func (c *Client) SetExchangeClient(client *remoting.ExchangeClient) {
	c.ExchangeClient = client
}
func (c *Client) SetResponseHandler(responseHandler remoting.ResponseHandler) {
	c.responseHandler = responseHandler
}
func (c *Client) Connect(url common.URL) {
	initClient(url.Protocol)
	c.conf = *clientConf
	// new client
	c.pool = newGettyRPCClientConnPool(c, clientConf.PoolSize, time.Duration(int(time.Second)*clientConf.PoolTTL))
	// codec
	c.codec = remoting.GetCodec(url.Protocol)
	c.addr = url.Location
}
func (c *Client) Close() {
	if c.pool != nil {
		c.pool.close()
	}
	c.pool = nil
}
func (c *Client) Request(request *remoting.Request, timeout time.Duration, callback common.AsyncCallback, response *remoting.PendingResponse) error {

	//p := &DubboPackage{}
	//p.Service.Path = strings.TrimPrefix(request.svcUrl.Path, "/")
	//p.Service.Interface = request.svcUrl.GetParam(constant.INTERFACE_KEY, "")
	//p.Service.Version = request.svcUrl.GetParam(constant.VERSION_KEY, "")
	//p.Service.Group = request.svcUrl.GetParam(constant.GROUP_KEY, "")
	//p.Service.Method = request.method
	//
	//p.Service.Timeout = c.opts.RequestTimeout
	//var timeout = request.svcUrl.GetParam(strings.Join([]string{constant.METHOD_KEYS, request.method + constant.RETRIES_KEY}, "."), "")
	//if len(timeout) != 0 {
	//	if t, err := time.ParseDuration(timeout); err == nil {
	//		p.Service.Timeout = t
	//	}
	//}
	//
	//p.Header.SerialID = byte(S_Dubbo)
	//p.Body = hessian.NewRequest(request.args, request.atta)
	//
	//var rsp *PendingResponse
	//if ct != CT_OneWay {
	//	p.Header.Type = hessian.PackageRequest_TwoWay
	//	rsp = NewPendingResponse()
	//	rsp.response = response
	//	rsp.callback = callback
	//} else {
	//	p.Header.Type = hessian.PackageRequest
	//}

	var (
		err     error
		session getty.Session
		conn    *gettyRPCClient
	)
	conn, session, err = c.selectSession(c.addr)
	if err != nil {
		return perrors.WithStack(err)
	}
	if session == nil {
		return errSessionNotExist
	}
	defer func() {
		if err == nil {
			c.pool.put(conn)
			return
		}
		conn.close()
	}()

	if err = c.transfer(session, request, timeout, response); err != nil {
		return perrors.WithStack(err)
	}

	if !request.TwoWay || callback != nil {
		return nil
	}

	select {
	case <-getty.GetTimeWheel().After(timeout):
		return perrors.WithStack(errClientReadTimeout)
	case <-response.Done:
		err = response.Err
	}

	return perrors.WithStack(err)
}

// CallOneway call one way
//func (c *Client) CallOneway(request *Request) error {
//
//	return perrors.WithStack(c.call(CT_OneWay, request, NewResponse(nil, nil), nil))
//}
//
//// Call if @response is nil, the transport layer will get the response without notify the invoker.
//func (c *Client) Call(request *Request, response *Response) error {
//
//	ct := CT_TwoWay
//	if response.reply == nil {
//		ct = CT_OneWay
//	}
//
//	return perrors.WithStack(c.call(ct, request, response, nil))
//}
//
//// AsyncCall ...
//func (c *Client) AsyncCall(request *Request, callback common.AsyncCallback, response *Response) error {
//
//	return perrors.WithStack(c.call(CT_TwoWay, request, response, callback))
//}
//
//func (c *Client) call(ct CallType, request *Request, response *Response, callback common.AsyncCallback) error {
//
//	p := &DubboPackage{}
//	p.Service.Path = strings.TrimPrefix(request.svcUrl.Path, "/")
//	p.Service.Interface = request.svcUrl.GetParam(constant.INTERFACE_KEY, "")
//	p.Service.Version = request.svcUrl.GetParam(constant.VERSION_KEY, "")
//	p.Service.Group = request.svcUrl.GetParam(constant.GROUP_KEY, "")
//	p.Service.Method = request.method
//
//	p.Service.Timeout = c.opts.RequestTimeout
//	var timeout = request.svcUrl.GetParam(strings.Join([]string{constant.METHOD_KEYS, request.method + constant.RETRIES_KEY}, "."), "")
//	if len(timeout) != 0 {
//		if t, err := time.ParseDuration(timeout); err == nil {
//			p.Service.Timeout = t
//		}
//	}
//
//	p.Header.SerialID = byte(S_Dubbo)
//	p.Body = hessian.NewRequest(request.args, request.atta)
//
//	var rsp *PendingResponse
//	if ct != CT_OneWay {
//		p.Header.Type = hessian.PackageRequest_TwoWay
//		rsp = NewPendingResponse()
//		rsp.response = response
//		rsp.callback = callback
//	} else {
//		p.Header.Type = hessian.PackageRequest
//	}
//
//	var (
//		err     error
//		session getty.Session
//		conn    *gettyRPCClient
//	)
//	conn, session, err = c.selectSession(request.addr)
//	if err != nil {
//		return perrors.WithStack(err)
//	}
//	if session == nil {
//		return errSessionNotExist
//	}
//	defer func() {
//		if err == nil {
//			c.pool.put(conn)
//			return
//		}
//		conn.close()
//	}()
//
//	if err = c.transfer(session, p, rsp); err != nil {
//		return perrors.WithStack(err)
//	}
//
//	if ct == CT_OneWay || callback != nil {
//		return nil
//	}
//
//	select {
//	case <-getty.GetTimeWheel().After(c.opts.RequestTimeout):
//		c.removePendingResponse(SequenceType(rsp.seq))
//		return perrors.WithStack(errClientReadTimeout)
//	case <-rsp.done:
//		err = rsp.err
//	}
//
//	return perrors.WithStack(err)
//}

func (c *Client) selectSession(addr string) (*gettyRPCClient, getty.Session, error) {
	rpcClient, err := c.pool.getGettyRpcClient(addr)
	if err != nil {
		return nil, nil, perrors.WithStack(err)
	}
	return rpcClient, rpcClient.selectSession(), nil
}

func (c *Client) heartbeat(session getty.Session) error {
	req := remoting.NewRequest("2.0.2")
	req.TwoWay = true
	req.Event = true
	resp := remoting.NewPendingResponse(req.Id)
	remoting.AddPendingResponse(resp)
	return c.transfer(session, req, 3*time.Second, resp)
}

func (c *Client) transfer(session getty.Session, request *remoting.Request, timeout time.Duration,
	rsp *remoting.PendingResponse) error {
	//sequence = c.sequence.Add(1)
	//
	//if pkg == nil {
	//	pkg = &DubboPackage{}
	//	pkg.Body = hessian.NewRequest([]interface{}{}, nil)
	//	pkg.Body = []interface{}{}
	//	pkg.Header.Type = hessian.PackageHeartbeat
	//	pkg.Header.SerialID = byte(S_Dubbo)
	//}
	//pkg.Header.ID = int64(sequence)

	// cond1
	//if rsp != nil {
	//	c.addPendingResponse(rsp)
	//}

	err := session.WritePkg(request, timeout)
	if rsp != nil { // cond2
		// cond2 should not merged with cond1. cause the response package may be returned very
		// soon and it will be handled by other goroutine.
		rsp.ReadStart = time.Now()
	}

	return perrors.WithStack(err)
}

//
//func (c *Client) addPendingResponse(pr *PendingResponse) {
//	c.pendingResponses.Store(SequenceType(pr.seq), pr)
//}
//
//func (c *Client) removePendingResponse(seq SequenceType) *PendingResponse {
//	if c.pendingResponses == nil {
//		return nil
//	}
//	if presp, ok := c.pendingResponses.Load(seq); ok {
//		c.pendingResponses.Delete(seq)
//		return presp.(*PendingResponse)
//	}
//	return nil
//}
