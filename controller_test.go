package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

type testMetrics map[string]float64

func (metrics testMetrics) Loads(context.Context, []Host) (map[string]float64, error) {
	return metrics, nil
}

func TestServersFiltersBySpotTagAcrossPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/servers/detail" || request.URL.Query().Get("tags") != "spot_instance:true" || request.URL.Query().Get("all_tenants") != "1" {
			t.Errorf("unexpected request: %s", request.URL.String())
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("marker") {
		case "":
			writer.Write([]byte(`{"servers":[{"id":"spot-1"}]}`))
		case "spot-1":
			writer.Write([]byte(`{"servers":[{"id":"spot-2"}]}`))
		case "spot-2":
			writer.Write([]byte(`{"servers":[]}`))
		default:
			t.Errorf("unexpected marker: %s", request.URL.Query().Get("marker"))
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	client := &gophercloud.ServiceClient{ProviderClient: &gophercloud.ProviderClient{TokenID: "test"}, Endpoint: server.URL + "/", Type: "compute", Microversion: "2.46"}
	controller := Controller{Compute: client}
	servers, err := controller.servers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || len(servers) != 2 || servers[0].ID != "spot-1" || servers[1].ID != "spot-2" {
		t.Fatalf("unexpected result: requests=%d servers=%+v", requests, servers)
	}
}

func TestControllerMigrationAndPending(t *testing.T) {
	requests := 0
	migrationStatus := "running"
	flavorID := "spot"
	var migrationBody map[string]map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case path == "/os-hypervisors/detail":
			if request.Header.Get("OpenStack-API-Version") != "compute 2.68" && request.Header.Get("X-OpenStack-Nova-API-Version") != "2.68" {
				t.Errorf("wrong hypervisor microversion: %v", request.Header)
			}
			if request.URL.Query().Get("marker") != "" {
				writer.Write([]byte(`{"hypervisors":[]}`))
				return
			}
			writer.Write([]byte(`{"hypervisors":[{"id":"src","host_ip":"192.168.20.250","state":"up","status":"enabled","service":{"host":"host-a"}},{"id":"dst","host_ip":"192.168.20.200","state":"up","status":"enabled","service":{"host":"host-b"}}]}`))
		case path == "/api/v1/query":
			json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
				map[string]any{"metric": map[string]string{"instance": "192.168.20.250:9100"}, "value": []any{time.Now().Unix(), "90"}},
				map[string]any{"metric": map[string]string{"instance": "192.168.20.200:9100"}, "value": []any{time.Now().Unix(), "20"}},
			}}})
		case strings.HasSuffix(path, "/inventories"):
			writer.Write([]byte(`{"inventories":{"VCPU":{"total":16,"reserved":0,"allocation_ratio":4,"min_unit":1,"max_unit":16,"step_size":1},"MEMORY_MB":{"total":8192,"reserved":1024,"allocation_ratio":1,"min_unit":1,"max_unit":8192,"step_size":1}}}`))
		case strings.HasSuffix(path, "/usages"):
			writer.Write([]byte(`{"usages":{"VCPU":4,"MEMORY_MB":1024}}`))
		case path == "/servers/detail":
			if request.URL.Query().Get("tags") != "spot_instance:true" {
				t.Errorf("missing spot instance tag filter: %s", request.URL.RawQuery)
			}
			if request.URL.Query().Get("marker") != "" {
				writer.Write([]byte(`{"servers":[]}`))
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"servers": []any{map[string]any{"id": "vm1", "status": "ACTIVE", "OS-EXT-SRV-ATTR:host": "host-a", "OS-EXT-STS:task_state": nil, "flavor": map[string]string{"id": flavorID}}}})
		case path == "/allocations/vm1":
			writer.Write([]byte(`{"allocations":{"src":{"resources":{"VCPU":2,"MEMORY_MB":2048}}}}`))
		case path == "/servers/vm1/action":
			requests++
			if err := json.NewDecoder(request.Body).Decode(&migrationBody); err != nil {
				t.Error(err)
			}
			writer.WriteHeader(http.StatusAccepted)
		case path == "/os-migrations":
			json.NewEncoder(writer).Encode(map[string]any{"migrations": []map[string]string{{"status": migrationStatus, "created_at": time.Now().UTC().Format(time.RFC3339Nano)}}})
		default:
			t.Errorf("unexpected request %s", path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	provider := &gophercloud.ProviderClient{TokenID: "test"}
	client := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/", Type: "compute", Microversion: "2.46"}
	placement := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/", Type: "placement"}
	settings := config{Flavors: map[string]bool{"spot": true}, Threshold: 80, MinImprovement: 20, Cooldown: time.Hour, DryRun: true, PrometheusURL: server.URL, PrometheusQuery: "cpu", MaxAge: time.Minute}
	controller := Controller{Compute: client, Placement: placement, Settings: settings, Metrics: prometheusMetrics{settings: settings, client: server.Client()}, LastMove: map[string]time.Time{}}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatal("dry run sent a migration")
	}
	controller.Settings.DryRun = false
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || migrationBody["os-migrateLive"]["host"] != "host-b" {
		t.Fatalf("unexpected migration: %#v", migrationBody)
	}
	if _, forced := migrationBody["os-migrateLive"]["force"]; forced {
		t.Fatal("forced migration is unsafe")
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || controller.PendingServer == "" {
		t.Fatal("controller did not wait for migration")
	}
	migrationStatus = "completed"
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.PendingServer != "" || requests != 1 {
		t.Fatal("completed migration was not cleared or cooldown was ignored")
	}
	flavorID = "other"
	var output bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `flavor_id="other" is not allowlisted`) || requests != 1 {
		t.Fatalf("expected flavor rejection without migration, got: %s", output.String())
	}
	flavorID = "spot"
	delete(controller.LastMove, "vm1")
	controller.Settings.Threshold = 5
	output.Reset()
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !strings.Contains(output.String(), "no eligible migration destination") || strings.Contains(output.String(), "proposal server=") {
		t.Fatalf("expected no eligible target and no proposal, requests=%d logs=%s", requests, output.String())
	}
}

func TestControllerShelvesWhenNoMigrationTargetIsEligible(t *testing.T) {
	serverStatus := "ACTIVE"
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case path == "/os-hypervisors/detail":
			if request.URL.Query().Get("marker") != "" {
				writer.Write([]byte(`{"hypervisors":[]}`))
				return
			}
			writer.Write([]byte(`{"hypervisors":[{"id":"src","host_ip":"192.168.20.250","state":"up","status":"enabled","service":{"host":"host-a"}},{"id":"dst","host_ip":"192.168.20.200","state":"up","status":"enabled","service":{"host":"host-b"}}]}`))
		case path == "/api/v1/query":
			json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
				map[string]any{"metric": map[string]string{"instance": "192.168.20.250:9100"}, "value": []any{time.Now().Unix(), "90"}},
				map[string]any{"metric": map[string]string{"instance": "192.168.20.200:9100"}, "value": []any{time.Now().Unix(), "20"}},
			}}})
		case strings.HasSuffix(path, "/inventories"):
			writer.Write([]byte(`{"inventories":{"VCPU":{"total":16,"reserved":0,"allocation_ratio":4,"min_unit":1,"max_unit":16,"step_size":1},"MEMORY_MB":{"total":8192,"reserved":1024,"allocation_ratio":1,"min_unit":1,"max_unit":8192,"step_size":1}}}`))
		case strings.HasSuffix(path, "/usages"):
			writer.Write([]byte(`{"usages":{"VCPU":4,"MEMORY_MB":1024}}`))
		case path == "/servers/detail":
			if request.URL.Query().Get("tags") != "spot_instance:true" {
				t.Errorf("missing spot server tag filter")
			}
			if request.URL.Query().Get("marker") != "" {
				writer.Write([]byte(`{"servers":[]}`))
				return
			}
			json.NewEncoder(writer).Encode(map[string]any{"servers": []any{map[string]any{"id": "vm1", "status": serverStatus, "OS-EXT-SRV-ATTR:host": "host-a", "OS-EXT-STS:task_state": nil, "flavor": map[string]string{"id": "spot"}}}})
		case path == "/allocations/vm1":
			writer.Write([]byte(`{"allocations":{"src":{"resources":{"VCPU":2,"MEMORY_MB":2048}}}}`))
		case path == "/servers/vm1/action":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["os-migrateLive"]; ok {
				t.Error("migration should not be requested when no destination fits")
			}
			if _, ok := body["shelve"]; ok {
				actions = append(actions, "shelve")
				serverStatus = "SHELVED"
			} else if _, ok := body["shelveOffload"]; ok {
				actions = append(actions, "shelveOffload")
				serverStatus = "SHELVED_OFFLOADED"
			} else {
				t.Errorf("unexpected server action: %v", body)
			}
			writer.WriteHeader(http.StatusAccepted)
		case path == "/servers/vm1":
			json.NewEncoder(writer).Encode(map[string]any{"server": map[string]any{"id": "vm1", "status": serverStatus, "OS-EXT-STS:task_state": nil}})
		default:
			t.Errorf("unexpected request %s", request.URL.String())
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	provider := &gophercloud.ProviderClient{TokenID: "test"}
	compute := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/", Type: "compute", Microversion: "2.46"}
	placement := &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: server.URL + "/", Type: "placement"}
	settings := config{Flavors: map[string]bool{"spot": true}, Threshold: 5, MinImprovement: 20, Cooldown: time.Hour, PrometheusURL: server.URL, PrometheusQuery: "cpu", MaxAge: time.Minute, DryRun: true, ShelveWhenNoDestination: true}
	controller := Controller{Compute: compute, Placement: placement, Settings: settings, Metrics: prometheusMetrics{settings: settings, client: server.Client()}, LastMove: map[string]time.Time{}}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 || controller.PendingServer != "" {
		t.Fatalf("dry run must not shelve, actions=%v pending=%q", actions, controller.PendingServer)
	}
	controller.Settings.DryRun = false
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0] != "shelve" || controller.PendingAction != "shelve" {
		t.Fatalf("expected shelve request, actions=%v pending=%q", actions, controller.PendingAction)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[1] != "shelveOffload" || controller.PendingAction != "offload" {
		t.Fatalf("expected offload after SHELVED, actions=%v pending=%q", actions, controller.PendingAction)
	}
	if err := controller.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.PendingServer != "" || len(actions) != 2 {
		t.Fatalf("expected completed offload, actions=%v pending=%q", actions, controller.PendingServer)
	}
}

func TestFitsRejectsInsufficientOrMissingInventory(t *testing.T) {
	host := Host{Inventory: map[string]Inventory{"MEMORY_MB": {Total: 4096, Reserved: 1024, AllocationRatio: 1, MinUnit: 1}}, Usage: map[string]int{"MEMORY_MB": 2048}}
	if fits(host, map[string]int{"MEMORY_MB": 1025}) || fits(host, map[string]int{"VCPU": 1}) || !fits(host, map[string]int{"MEMORY_MB": 1024}) {
		t.Fatal("incorrect destination capacity check")
	}
}

func TestServerIneligibility(t *testing.T) {
	controller := Controller{Settings: config{Flavors: map[string]bool{"spot": true}, Cooldown: time.Hour}, LastMove: map[string]time.Time{}}
	source := Host{Name: "compute-a"}
	server := Server{ID: "server-1", Host: "compute-a", Status: "ACTIVE"}
	server.Flavor.ID = "spot"
	if reason := controller.serverIneligibility(server, source); reason != "" {
		t.Fatalf("eligible server rejected: %s", reason)
	}
	task := "migrating"
	for _, test := range []struct {
		name   string
		change func(*Server)
		want   string
	}{
		{"host", func(server *Server) { server.Host = "compute-b" }, `host="compute-b"`},
		{"status", func(server *Server) { server.Status = "SHUTOFF" }, `status="SHUTOFF"`},
		{"task", func(server *Server) { server.TaskState = &task }, `task_state="migrating"`},
		{"flavor", func(server *Server) { server.Flavor.ID = "other" }, `flavor_id="other"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := server
			test.change(&candidate)
			if reason := controller.serverIneligibility(candidate, source); !strings.Contains(reason, test.want) {
				t.Fatalf("reason %q does not contain %q", reason, test.want)
			}
		})
	}
	controller.LastMove[server.ID] = time.Now()
	if reason := controller.serverIneligibility(server, source); !strings.Contains(reason, "cooldown") {
		t.Fatalf("expected cooldown reason, got %q", reason)
	}
}
