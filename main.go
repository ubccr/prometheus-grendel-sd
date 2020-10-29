package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/ubccr/grendel/client"
	gmodel "github.com/ubccr/grendel/model"
)

var (
	a                = kingpin.New("prometheus-grendel-sd", "Tool to generate file_sd target files for Grendel.")
	outputFile       = a.Flag("output.file", "Output file for file_sd compatible file.").Default("grendel_sd.json").String()
	refresh          = a.Flag("target.refresh", "The refresh interval (in seconds).").Default("60").Int()
	nodeExporterPort = a.Flag("grendel.node_exporter_port", "Node exporter port").Default("9100").Int()
	apiEndpoint      = a.Flag("grendel.endpoint", "The address the grendel HTTP API is listening on for requests.").Default("/var/grendel/grendel-api.socket").String()
	customCA         = a.Flag("grendel.capath", "Path to grendel custom CA").Default("").String()
	logger           log.Logger
)

type grendelDiscoverer struct {
	client          *client.APIClient
	refreshInterval int
	logger          log.Logger
}

func (d *grendelDiscoverer) parseHosts(hostList gmodel.HostList) (*targetgroup.Group, error) {
	tgroup := targetgroup.Group{
		Source: "grendel",
		Labels: make(model.LabelSet),
	}

	tgroup.Targets = make([]model.LabelSet, 0, len(hostList))

	for _, host := range hostList {
		var primaryNic *gmodel.NetInterface
		for _, nic := range host.Interfaces {
			if !nic.BMC {
				primaryNic = nic
				break
			}
		}

		if primaryNic == nil {
			continue
		}

		var addr string
		if primaryNic.FQDN != "" {
			addr = net.JoinHostPort(primaryNic.FQDN, fmt.Sprintf("%d", *nodeExporterPort))
		} else {
			addr = net.JoinHostPort(primaryNic.IP.String(), fmt.Sprintf("%d", *nodeExporterPort))
		}

		target := model.LabelSet{model.AddressLabel: model.LabelValue(addr)}
		labels := model.LabelSet{
			model.LabelName("job"): model.LabelValue("compute"),
		}
		tgroup.Labels = labels

		tgroup.Targets = append(tgroup.Targets, target)
	}
	return &tgroup, nil
}

func (d *grendelDiscoverer) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	for c := time.Tick(time.Duration(d.refreshInterval) * time.Second); ; {
		hostList, _, err := d.client.HostApi.HostList(context.Background())
		if err != nil {
			level.Error(d.logger).Log("msg", "Error getting node list", "err", err)
			time.Sleep(time.Duration(d.refreshInterval) * time.Second)
			continue
		}

		tg, err := d.parseHosts(hostList)
		if err != nil {
			level.Error(d.logger).Log("msg", "Error parsing hostlist", "err", err)
			break
		}

		tgs := []*targetgroup.Group{tg}
		if err == nil {
			ch <- tgs
		}
		// Wait for ticker or exit when ctx is closed.
		select {
		case <-c:
			continue
		case <-ctx.Done():
			return
		}
	}
}

func newGrendelDiscoverer() (*grendelDiscoverer, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

	pem, err := ioutil.ReadFile(*customCA)
	if err == nil {
		certPool := x509.NewCertPool()
		if !certPool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("Failed to read cacert: %s", *customCA)
		}

		tr = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: certPool, InsecureSkipVerify: false}}
	}

	// Is endpoint a path to a unix domain socket?
	if !strings.HasPrefix(*apiEndpoint, "http://") && !strings.HasPrefix(*apiEndpoint, "https://") {
		tr = &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				dialer := net.Dialer{}
				return dialer.DialContext(ctx, "unix", *apiEndpoint)
			},
		}
	}

	rclient := retryablehttp.NewClient()
	rclient.HTTPClient = &http.Client{Timeout: time.Second * 3600, Transport: tr}

	cfg := client.NewConfiguration()
	cfg.HTTPClient = rclient.StandardClient()

	client := client.NewAPIClient(cfg)

	cd := &grendelDiscoverer{
		client:          client,
		refreshInterval: *refresh,
		logger:          logger,
	}
	return cd, nil
}

func main() {
	a.HelpFlag.Short('h')

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Println("err: ", err)
		return
	}
	logger = log.NewSyncLogger(log.NewLogfmtLogger(os.Stdout))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)

	disc, err := newGrendelDiscoverer()
	if err != nil {
		fmt.Println("err: ", err)
	}

	ctx := context.Background()
	sdAdapter := NewAdapter(ctx, *outputFile, "grendelSD", disc, logger)
	sdAdapter.Run()

	<-ctx.Done()
}
