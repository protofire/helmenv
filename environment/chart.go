package environment

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	v1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"math/rand"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"helm.sh/helm/v3/pkg/action"
)

const (
	// MaxPort max port value for forwarding
	MaxPort = 50000
	// MinPort min port value for forwarding
	MinPort = 20000
)

// ChartSettings chart settings config
type ChartSettings struct {
	ReleaseName string                     `json:"release_name"`
	Path        string                     `json:"path"`
	Values      map[string]interface{}     `json:"values"`
	PodsInfo    map[string]*ConnectionInfo `json:"pods_info"`
}

// HelmChart helm chart structure
type HelmChart struct {
	Name          string
	NamespaceName string
	settings      *ChartSettings
	env           *HelmEnvironment
	actionConfig  *action.Configuration
	podsList      *v1.PodList
}

// NewHelmChart creates a new Helm chart wrapper
func NewHelmChart(env *HelmEnvironment, cfg *ChartSettings) (*HelmChart, error) {
	hc := &HelmChart{
		Name:          cfg.ReleaseName,
		env:           env,
		settings:      cfg,
		NamespaceName: env.Config.NamespaceName,
	}
	if err := hc.initAction(); err != nil {
		return nil, err
	}
	return hc, nil
}

// Connect connects to all exposed containerPorts, forwards them to local
func (hc *HelmChart) Connect() error {
	for _, connectionInfo := range hc.settings.PodsInfo {
		rules := make([]string, 0)
		for _, p := range connectionInfo.Ports {
			freePort := rand.Intn(MaxPort-MinPort) + MinPort
			if p.Name == "" {
				return fmt.Errorf("port %d must be named in helm chart", p.ContainerPort)
			}
			connectionInfo.LocalPorts[p.Name] = freePort
			rules = append(rules, fmt.Sprintf("%d:%d", freePort, p.ContainerPort))
		}
		if len(rules) > 0 {
			if hc.env.Config.Persistent {
				pid, err := hc.env.runProcessForwarder(connectionInfo.PodName, rules)
				if err != nil {
					return err
				}
				connectionInfo.ForwarderPID = pid
			} else {
				if err := hc.env.runGoForwarder(connectionInfo.PodName, rules); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Deploy deploys a chart and update config settings
func (hc *HelmChart) Deploy() error {
	if err := hc.DeployChart(); err != nil {
		return err
	}
	if err := hc.enumerateApps(); err != nil {
		return err
	}
	if err := hc.fetchPods(); err != nil {
		return err
	}
	if err := hc.updateChartSettings(); err != nil {
		return err
	}
	return nil
}

func (hc *HelmChart) initAction() error {
	hc.actionConfig = &action.Configuration{}
	if err := hc.actionConfig.Init(
		hc.env.CLISettings.RESTClientGetter(),
		hc.NamespaceName,
		"configmap",
		func(format string, v ...interface{}) {
			log.Debug().Str("LogType", "Helm").Msg(fmt.Sprintf(format, v...))
		}); err != nil {
		return err
	}
	return nil
}

// DeployChart deploys the helm Charts
func (hc *HelmChart) DeployChart() error {
	log.Info().Str("Path", hc.settings.Path).
		Str("Release", hc.settings.ReleaseName).
		Str("Namespace", hc.NamespaceName).
		Msg("Installing Helm chart")
	chart, err := loader.Load(hc.settings.Path)
	if err != nil {
		return err
	}

	chart.Values, err = chartutil.CoalesceValues(chart, hc.settings.Values)
	if err != nil {
		return err
	}

	install := action.NewInstall(hc.actionConfig)
	install.Namespace = hc.NamespaceName
	install.ReleaseName = hc.settings.ReleaseName
	install.Timeout = HelmInstallTimeout
	// blocks until all podsPortsInfo are healthy
	install.Wait = true
	_, err = install.Run(chart, nil)
	if err != nil {
		return err
	}
	log.Info().
		Str("Namespace", hc.NamespaceName).
		Str("Release", hc.settings.ReleaseName).
		Str("Chart", hc.settings.Path).
		Msg("Succesfully installed helm chart")
	return nil
}

func (hc *HelmChart) fetchPods() error {
	var err error
	k8sPods := hc.env.k8sClient.CoreV1().Pods(hc.NamespaceName)
	hc.podsList, err = k8sPods.List(context.Background(), metaV1.ListOptions{
		LabelSelector: fmt.Sprintf("release=%s", hc.settings.ReleaseName),
	})
	if err != nil {
		return err
	}
	return nil
}

func (hc *HelmChart) addInstanceLabel(app string) error {
	k8sPods := hc.env.k8sClient.CoreV1().Pods(hc.NamespaceName)
	l, err := k8sPods.List(context.Background(), metaV1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", app)})
	if err != nil {
		return err
	}
	for i, pod := range l.Items {
		labelPatch := fmt.Sprintf(`[{"op":"add","path":"/metadata/labels/%s","value":"%d" }]`, "instance", i)
		_, err := k8sPods.Patch(context.Background(), pod.GetName(), types.JSONPatchType, []byte(labelPatch), metaV1.PatchOptions{})
		if err != nil {
			return errors.Wrapf(err, "failed to update labels %s for pod %s", labelPatch, pod.Name)
		}
	}
	return nil
}

func (hc *HelmChart) updateChartSettings() error {
	for _, p := range hc.podsList.Items {
		for _, c := range p.Spec.Containers {
			if hc.settings.PodsInfo == nil {
				hc.settings.PodsInfo = make(map[string]*ConnectionInfo)
			}
			app, ok := p.Labels["app"]
			if !ok {
				log.Warn().Str("Container", c.Name).Msg("App label not found")
			}
			instance := p.Labels["instance"]
			if !ok {
				log.Warn().Str("Container", c.Name).Msg("Instance label not found")
			}
			log.Info().
				Str("Container", c.Name).
				Interface("PodPorts", c.Ports).
				Msg("Container info")
			instanceKey := fmt.Sprintf("%s:%s", app, instance)
			if _, ok := hc.settings.PodsInfo[instanceKey]; ok {
				return fmt.Errorf("ambiguous instance key: %s", instanceKey)
			}
			pm := map[string]v1.ContainerPort{}
			for _, port := range c.Ports {
				pm[port.Name] = port
			}
			hc.settings.PodsInfo[instanceKey] = &ConnectionInfo{
				PodName:    p.Name,
				PodIP:      p.Status.PodIP,
				Ports:      pm,
				LocalPorts: make(map[string]int),
			}
		}
	}
	return nil
}

func (hc *HelmChart) enumerateApps() error {
	apps, err := hc.uniqueAppLabels("app")
	if err != nil {
		return err
	}
	for _, app := range apps {
		if err := hc.addInstanceLabel(app); err != nil {
			return err
		}
	}
	return nil
}

func (hc *HelmChart) uniqueAppLabels(selector string) ([]string, error) {
	uniqueLabels := make([]string, 0)
	isUnique := make(map[string]bool)
	k8sPods := hc.env.k8sClient.CoreV1().Pods(hc.NamespaceName)
	podList, err := k8sPods.List(context.Background(), metaV1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "no labels with selector %s found for enumeration", selector)
	}
	for _, p := range podList.Items {
		appLabel := p.Labels["app"]
		if _, ok := isUnique[appLabel]; !ok {
			uniqueLabels = append(uniqueLabels, appLabel)
		}
	}
	log.Info().
		Interface("AppLabels", uniqueLabels).
		Msg("Apps found")
	return uniqueLabels, nil
}

func (k *HelmEnvironment) killForwarder(pid int) error {
	if pid == 0 {
		return nil
	}
	cmd := exec.Command(
		"kill",
		"-9",
		strconv.Itoa(pid),
	)
	err := cmd.Start()
	if err != nil {
		return errors.Wrapf(err, "failed to kill forwarder with pid: %d", pid)
	}
	return nil
}

func (k *HelmEnvironment) runProcessForwarder(podName string, portRules []string) (int, error) {
	cmd := exec.Command(
		"kubectl",
		"-n", k.Config.NamespaceName,
		"port-forward",
		fmt.Sprintf("%s/%s", "pods", podName),
		strings.Join(portRules, " "),
	)
	err := cmd.Start()
	if err != nil {
		return 0, errors.Wrapf(err, "failed to forward using cmd: %s", cmd.String())
	}
	log.Debug().
		Str("Command", cmd.String()).
		Msg("Forwarded ports")
	return cmd.Process.Pid, nil
}

func (k *HelmEnvironment) runGoForwarder(podName string, portRules []string) error {
	roundTripper, upgrader, err := spdy.RoundTripperFor(k.k8sConfig)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", k.Config.NamespaceName, podName)
	hostIP := strings.TrimLeft(k.k8sConfig.Host, "htps:/")
	serverURL := url.URL{Scheme: "https", Path: path, Host: hostIP}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, &serverURL)

	stopChan, readyChan := make(chan struct{}, 1), make(chan struct{}, 1)
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)

	log.Debug().
		Str("Pod", podName).
		Msg("Attempting to forward port")

	forwarder, err := portforward.New(dialer, portRules, stopChan, readyChan, out, errOut)
	if err != nil {
		return err
	}
	go func() {
		if err := forwarder.ForwardPorts(); err != nil {
			log.Error().Str("Pod", podName).Err(err)
		}
	}()

	log.Debug().Str("Pod", podName).Msg("Waiting on podsPortsInfo forwarded forwardingRuleStrings to be ready")
	<-readyChan
	if len(errOut.String()) > 0 {
		return fmt.Errorf("error on forwarding k8s port: %v", errOut.String())
	}
	if len(out.String()) > 0 {
		msg := strings.ReplaceAll(out.String(), "\n", " ")
		log.Debug().Str("Pod", podName).Msgf("%s", msg)
	}
	return nil
}
