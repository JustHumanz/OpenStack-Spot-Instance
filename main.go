package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	osConfig "github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"gopkg.in/yaml.v3"
)

type config struct {
	Driver                  string
	PrometheusURL           string
	PrometheusQuery         string
	Kubeconfig              string
	HostMap                 map[string]string
	Flavors                 map[string]bool
	Threshold               float64
	MinImprovement          float64
	Interval                time.Duration
	Cooldown                time.Duration
	MaxAge                  time.Duration
	DryRun                  bool
	ShelveWhenNoDestination bool
	Once                    bool
}

type configFile struct {
	Metrics struct {
		Driver          string            `yaml:"driver"`
		PrometheusURL   string            `yaml:"prometheus_url"`
		PrometheusQuery string            `yaml:"prometheus_query"`
		Kubeconfig      string            `yaml:"kubeconfig"`
		HostMap         map[string]string `yaml:"host_map"`
	} `yaml:"metrics"`
	Spot struct {
		Flavors                 []string `yaml:"flavors"`
		Threshold               *float64 `yaml:"threshold"`
		MinImprovement          *float64 `yaml:"min_improvement"`
		ShelveWhenNoDestination *bool    `yaml:"shelve_when_no_destination"`
	} `yaml:"spot"`
	Controller struct {
		Interval      string `yaml:"interval"`
		Cooldown      string `yaml:"cooldown"`
		MaxMetricsAge string `yaml:"max_metrics_age"`
		DryRun        *bool  `yaml:"dry_run"`
	} `yaml:"controller"`
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

func loadConfig() (config, error) {
	configPath := flag.String("config", "config.yaml", "path to the controller YAML configuration")
	once := flag.Bool("once", false, "run one cycle")
	flag.Parse()
	result, err := loadConfigFile(*configPath)
	if err != nil {
		return result, err
	}
	result.Once = *once
	return result, nil
}

func loadConfigFile(path string) (config, error) {
	result := config{
		Driver:                  "placement",
		PrometheusQuery:         `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
		HostMap:                 map[string]string{},
		Flavors:                 map[string]bool{},
		Threshold:               80,
		MinImprovement:          20,
		Interval:                time.Minute,
		Cooldown:                30 * time.Minute,
		MaxAge:                  3 * time.Minute,
		ShelveWhenNoDestination: true,
	}
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var source configFile
	if err := decoder.Decode(&source); err != nil {
		return result, fmt.Errorf("decode config %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return result, fmt.Errorf("decode config %q: multiple YAML documents are not supported", path)
		}
		return result, fmt.Errorf("decode config %q: %w", path, err)
	}
	if source.Metrics.Driver != "" {
		result.Driver = source.Metrics.Driver
	}
	result.PrometheusURL = source.Metrics.PrometheusURL
	if source.Metrics.PrometheusQuery != "" {
		result.PrometheusQuery = source.Metrics.PrometheusQuery
	}
	result.Kubeconfig = source.Metrics.Kubeconfig
	if source.Metrics.HostMap != nil {
		result.HostMap = source.Metrics.HostMap
	}
	for _, flavor := range source.Spot.Flavors {
		if flavor = strings.TrimSpace(flavor); flavor != "" {
			result.Flavors[flavor] = true
		}
	}
	if source.Spot.Threshold != nil {
		result.Threshold = *source.Spot.Threshold
	}
	if source.Spot.MinImprovement != nil {
		result.MinImprovement = *source.Spot.MinImprovement
	}
	if source.Spot.ShelveWhenNoDestination != nil {
		result.ShelveWhenNoDestination = *source.Spot.ShelveWhenNoDestination
	}
	if source.Controller.Interval != "" {
		result.Interval, err = time.ParseDuration(source.Controller.Interval)
		if err != nil {
			return result, fmt.Errorf("controller.interval: %w", err)
		}
	}
	if source.Controller.Cooldown != "" {
		result.Cooldown, err = time.ParseDuration(source.Controller.Cooldown)
		if err != nil {
			return result, fmt.Errorf("controller.cooldown: %w", err)
		}
	}
	if source.Controller.MaxMetricsAge != "" {
		result.MaxAge, err = time.ParseDuration(source.Controller.MaxMetricsAge)
		if err != nil {
			return result, fmt.Errorf("controller.max_metrics_age: %w", err)
		}
	}
	if source.Controller.DryRun != nil {
		result.DryRun = *source.Controller.DryRun
	}

	if len(result.Flavors) == 0 {
		return result, errors.New("spot.flavors must contain at least one flavor ID")
	}
	if result.Driver != "placement" && result.Driver != "prometheus" && result.Driver != "metrics-server" {
		return result, errors.New("invalid metrics driver")
	}

	if result.Threshold <= 0 || result.Threshold > 100 || result.MinImprovement <= 0 || result.MinImprovement > 100 || result.Interval <= 0 || result.Cooldown <= 0 || result.MaxAge <= 0 {
		return result, errors.New("invalid threshold, improvement, or duration")
	}

	switch result.Driver {
	case "prometheus":
		if result.PrometheusURL == "" {
			return result, errors.New("PROMETHEUS_URL is required")
		}
	case "metrics-server":
		if result.Kubeconfig == "" {
			_, err := os.Stat(filepath.Join(saDir, "token"))
			return result, err
		}
	}

	return result, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	settings, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	authOptions, _, tlsConfig, err := clouds.Parse()
	if err != nil {
		return err
	}

	authOptions.AllowReauth = true
	provider, err := osConfig.NewProviderClient(ctx, authOptions, osConfig.WithTLSConfig(tlsConfig))
	if err != nil {
		return err
	}
	endpoint := gophercloud.EndpointOpts{Region: os.Getenv("OS_REGION_NAME")}
	switch env("OS_INTERFACE", "public") {
	case "public":
		endpoint.Availability = gophercloud.AvailabilityPublic
	case "internal":
		endpoint.Availability = gophercloud.AvailabilityInternal
	case "admin":
		endpoint.Availability = gophercloud.AvailabilityAdmin
	default:
		return errors.New("invalid OS_INTERFACE")
	}
	compute, err := openstack.NewComputeV2(provider, endpoint)
	if err != nil {
		return err
	}
	placement, err := openstack.NewPlacementV1(provider, endpoint)
	if err != nil {
		return err
	}
	compute.Microversion = "2.46"
	placement.Microversion = "1.28"
	controller := Controller{Compute: compute, Placement: placement, Settings: settings, LastMove: map[string]time.Time{}}
	metrics, err := newMetrics(settings)
	if err != nil {
		return err
	}
	controller.Metrics = metrics
	log.Printf("metrics driver=%s", settings.Driver)

	flavorIDs := make([]string, 0, len(settings.Flavors))
	for flavorID := range settings.Flavors {
		flavorIDs = append(flavorIDs, flavorID)
	}
	sort.Strings(flavorIDs)
	log.Printf("spot flavor allowlist=%q", strings.Join(flavorIDs, ","))
	for {
		if err := controller.Cycle(ctx); err != nil && ctx.Err() == nil {
			if settings.Once {
				return fmt.Errorf("cycle failed: %w", err)
			}
			log.Printf("cycle failed: %v", err)
		}
		if settings.Once || ctx.Err() != nil {
			return ctx.Err()
		}
		timer := time.NewTimer(settings.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
