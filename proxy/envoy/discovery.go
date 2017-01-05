// Copyright 2016 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package envoy

import (
	"net/http"
	"strconv"

	restful "github.com/emicklei/go-restful"

	"istio.io/manager/model"
)

// DiscoveryService publishes services, clusters, and routes for proxies
type DiscoveryService struct {
	services model.ServiceDiscovery
	server   *http.Server
}

type hosts struct {
	Hosts []host `json:"hosts"`
}

type host struct {
	Address string `json:"ip_address"`
	Port    int    `json:"port"`
	// Weight is an integer in the range [1, 100] or empty
	Weight int `json:"load_balancing_weight,omitempty"`
}

func NewDiscoveryService(services model.ServiceDiscovery, port int) (*DiscoveryService, error) {
	out := DiscoveryService{
		services: services,
	}
	container := restful.NewContainer()
	out.Register(container)
	out.server = &http.Server{Addr: ":" + strconv.Itoa(port), Handler: container}

	return &out, nil
}

func (ds *DiscoveryService) Register(container *restful.Container) {
	ws := &restful.WebService{}

	ws.Route(ws.
		GET("/v1/registration/{service-key}").
		To(ds.ListEndpoints).
		Doc("SDS registration").
		Param(ws.PathParameter("service-key", "tuple of service name and tag name").DataType("string")).
		Writes(hosts{}))

	container.Add(ws)
}

func (ds *DiscoveryService) Run() error {
	return ds.server.ListenAndServe()
}

func (ds *DiscoveryService) ListEndpoints(request *restful.Request, response *restful.Response) {
	key := request.PathParameter("service-key")
	svc := model.ParseServiceString(key)

	var out []host
	for _, tag := range svc.Tags {
		instances := ds.services.Endpoints(svc, tag)
		for _, ep := range instances {
			out = append(out, host{
				Address: ep.Endpoint.Address,
				Port:    ep.Endpoint.Port,
			})
		}
	}

	response.WriteEntity(hosts{out})
}