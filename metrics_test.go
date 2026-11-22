package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	metricsv1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
)

func TestLoadConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `metrics:
  driver: metrics-server
  kubeconfig: /tmp/kubeconfig
  host_map:
    nova-compute-1: worker-1
spot:
  flavors:
    - " flavor-a "
    - flavor-b
  threshold: 5
  min_improvement: 8
  shelve_when_no_destination: false
controller:
  interval: 45s
  cooldown: 2h
  max_metrics_age: 90s
  dry_run: true
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Driver != "metrics-server" || settings.Kubeconfig != "/tmp/kubeconfig" || settings.HostMap["nova-compute-1"] != "worker-1" {
		t.Fatalf("metrics config not loaded: %+v", settings)
	}
	if !settings.Flavors["flavor-a"] || !settings.Flavors["flavor-b"] || len(settings.Flavors) != 2 {
		t.Fatalf("flavors not normalized: %v", settings.Flavors)
	}
	if settings.Threshold != 5 || settings.MinImprovement != 8 || settings.ShelveWhenNoDestination {
		t.Fatalf("spot config not loaded: %+v", settings)
	}
	if settings.Interval != 45*time.Second || settings.Cooldown != 2*time.Hour || settings.MaxAge != 90*time.Second || !settings.DryRun {
		t.Fatalf("controller config not loaded: %+v", settings)
	}
}

func TestLoadConfigFileDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spot:\n  flavors: [spot]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Driver != "placement" || settings.Threshold != 80 || settings.MinImprovement != 20 || settings.Interval != time.Minute || settings.Cooldown != 30*time.Minute || settings.MaxAge != 3*time.Minute || !settings.ShelveWhenNoDestination {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if err := os.WriteFile(path, []byte("metrics:\n  typo: true\nspot:\n  flavors: [spot]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(path); err == nil {
		t.Fatal("expected unknown YAML key to be rejected")
	}
}

func TestPrometheusMetricsMappingAndStaleness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/query" || request.URL.Query().Get("query") != "cpu" {
			t.Errorf("wrong query: %s", request.URL)
		}
		json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
			map[string]any{"metric": map[string]string{"instance": "node-a:9100"}, "value": []any{time.Now().Unix(), "85"}},
			map[string]any{"metric": map[string]string{"instance": "node-b:9100"}, "value": []any{time.Now().Add(-time.Hour).Unix(), "10"}},
		}}})
	}))
	defer server.Close()
	settings := config{PrometheusURL: server.URL, PrometheusQuery: "cpu", MaxAge: time.Minute, HostMap: map[string]string{"host-a": "node-a:9100", "host-b": "node-b:9100"}}
	loads, err := (prometheusMetrics{settings: settings, client: server.Client()}).Loads(context.Background(), []Host{{Name: "host-a"}, {Name: "host-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if loads["host-a"] != 85 || len(loads) != 1 {
		t.Fatalf("unexpected loads: %v", loads)
	}
}

func TestPrometheusMatchesNovaHostIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
			map[string]any{"metric": map[string]string{"instance": "192.168.20.250:9100"}, "value": []any{time.Now().Unix(), "99.98"}},
			map[string]any{"metric": map[string]string{"instance": "192.168.20.200:9100"}, "value": []any{time.Now().Unix(), "12.4"}},
		}}})
	}))
	defer server.Close()
	settings := config{PrometheusURL: server.URL, PrometheusQuery: "cpu", MaxAge: time.Minute}
	driver := prometheusMetrics{settings: settings, client: server.Client()}
	hosts := []Host{{Name: "compute-a", HostIP: "192.168.20.250"}, {Name: "compute-b", HostIP: "192.168.20.200"}}
	loads, err := driver.Loads(context.Background(), hosts)
	if err != nil || loads["compute-a"] != 99.98 || loads["compute-b"] != 12.4 {
		t.Fatalf("unexpected loads %v: %v", loads, err)
	}
	driver.settings.HostMap = map[string]string{"compute-a": "unmatched:9100"}
	loads, err = driver.Loads(context.Background(), hosts)
	if err != nil || len(loads) != 1 || loads["compute-b"] != 12.4 {
		t.Fatalf("explicit mapping must not fall back: %v: %v", loads, err)
	}
	hosts = append(hosts, Host{Name: "compute-c", HostIP: "192.168.20.200"})
	loads, err = driver.Loads(context.Background(), hosts)
	if err != nil || len(loads) != 0 {
		t.Fatalf("ambiguous host IP must not match: %v: %v", loads, err)
	}
}

func TestPrometheusURLForms(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected query path: %s", request.URL.Path)
		}
		json.NewEncoder(writer).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}})
	}))
	defer server.Close()
	for _, path := range []string{"", "/query", "/api/v1/query"} {
		t.Run(path, func(t *testing.T) {
			driver := prometheusMetrics{settings: config{PrometheusURL: server.URL + path, PrometheusQuery: "cpu"}, client: server.Client()}
			if _, err := driver.Loads(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestKubernetesMetricsUsesNodeCPUCapacity(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}}}
	sample := &metricsv1.NodeMetrics{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Timestamp: metav1.Now(), Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}
	metrics := metricsfake.NewSimpleClientset()
	metrics.PrependReactor("get", "nodes", func(action kubetesting.Action) (bool, runtime.Object, error) { return true, sample, nil })
	driver := kubernetesMetrics{settings: config{MaxAge: time.Minute, HostMap: map[string]string{"compute-a": "node-a"}}, core: fake.NewSimpleClientset(node), metrics: metrics}
	loads, err := driver.Loads(context.Background(), []Host{{Name: "compute-a"}})
	if err != nil || loads["compute-a"] != 50 {
		t.Fatalf("unexpected loads %v: %v", loads, err)
	}
}

func TestPlacementMetricsUsesAllocationRatio(t *testing.T) {
	host := Host{Name: "host-a", Inventory: map[string]Inventory{"VCPU": {Total: 16, Reserved: 2, AllocationRatio: 4}}, Usage: map[string]int{"VCPU": 28}}
	loads, err := (placementMetrics{}).Loads(context.Background(), []Host{host})
	if err != nil || loads["host-a"] != 50 {
		t.Fatalf("unexpected loads %v: %v", loads, err)
	}
}
