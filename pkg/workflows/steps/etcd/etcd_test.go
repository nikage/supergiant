package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/google/uuid"

	"github.com/supergiant/control/pkg/node"
	"github.com/supergiant/control/pkg/profile"
	"github.com/supergiant/control/pkg/runner"
	"github.com/supergiant/control/pkg/templatemanager"
	"github.com/supergiant/control/pkg/testutils"
	"github.com/supergiant/control/pkg/workflows/steps"
	"github.com/supergiant/control/pkg/workflows/steps/docker"
)

type fakeRunner struct {
	errMsg string
}

func (f *fakeRunner) Run(command *runner.Command) error {
	if len(f.errMsg) > 0 {
		return errors.New(f.errMsg)
	}

	_, err := io.Copy(command.Out, strings.NewReader(command.Script))
	return err
}

func TestInstallEtcD(t *testing.T) {
	err := templatemanager.Init("../../../../templates")

	if err != nil {
		t.Fatal(err)
	}

	tpl, _ := templatemanager.GetTemplate(StepName)

	if tpl == nil {
		t.Fatal("template not found")
	}

	host := "10.20.30.40"
	servicePort := "2379"
	managementPort := "2380"
	dataDir := "/var/data"
	version := "3.3.9"
	name := "etcd0"
	clusterToken := "tkn"

	r := &testutils.MockRunner{
		Err: nil,
	}

	output := &bytes.Buffer{}
	config := steps.NewConfig("", "", "", profile.Profile{})
	config.EtcdConfig = steps.EtcdConfig{
		Host:           host,
		ServicePort:    servicePort,
		ManagementPort: managementPort,
		DataDir:        dataDir,
		Version:        version,
		Name:           name,
		ClusterToken:   clusterToken,
		Timeout:        time.Second * 10,
		RestartTimeout: "5",
		StartTimeout:   "0",
	}
	config.IsMaster = true
	config.Runner = r
	config.Node = node.Node{
		ID:        uuid.New().String(),
		PrivateIp: "10.20.30.40",
		PublicIp:  "127.0.0.1",
	}

	config.AddMaster(&config.Node)
	config.AddMaster(&node.Node{
		PrivateIp: "0.0.0.0",
		ID:        uuid.New().String(),
		Name:      "etcd1",
	})

	task := &Step{
		script: tpl,
	}

	err = task.Run(context.Background(), output, config)

	d := output.String()
	fmt.Fprint(os.Stdout, d)
	if err != nil {
		t.Errorf("Unpexpected error %s", err.Error())
	}

	if !strings.Contains(output.String(), host) {
		t.Errorf("Master private ip %s not found in %s", host, output.String())
	}

	if !strings.Contains(output.String(), servicePort) {
		t.Errorf("Service port %s not found in %s", servicePort, output.String())
	}

	if !strings.Contains(output.String(), managementPort) {
		t.Errorf("Management port %s not found in %s", managementPort, output.String())
	}

	if !strings.Contains(output.String(), dataDir) {
		t.Errorf("data dir %s not found in %s", dataDir, output.String())
	}

	if !strings.Contains(output.String(), version) {
		t.Errorf("version %s not found in %s", version, output.String())
	}
}

func TestInstallEtcdTimeout(t *testing.T) {
	err := templatemanager.Init("../../../../templates")

	if err != nil {
		t.Fatal(err)
	}

	tpl, _ := templatemanager.GetTemplate(StepName)

	if tpl == nil {
		t.Fatal("template not found")
	}

	r := &testutils.MockRunner{
		Err: nil,
	}

	output := &bytes.Buffer{}
	config := steps.NewConfig("", "", "", profile.Profile{})
	config.EtcdConfig = steps.EtcdConfig{
		Timeout: time.Second * 0,
	}
	config.IsMaster = true
	config.Runner = r
	config.Node = node.Node{
		PrivateIp: "10.20.30.40",
	}

	task := &Step{
		script: tpl,
	}

	err = task.Run(context.Background(), output, config)

	if err == nil {
		t.Error("Error must not be nil")
	}

	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("deadline not found in error message %s", err.Error())
	}
}

func TestEtcdError(t *testing.T) {
	errMsg := "error has occurred"

	r := &fakeRunner{
		errMsg: errMsg,
	}

	proxyTemplate, err := template.New(StepName).Parse("")
	output := new(bytes.Buffer)

	task := &Step{
		proxyTemplate,
	}

	cfg := steps.NewConfig("", "", "", profile.Profile{})
	cfg.Runner = r
	err = task.Run(context.Background(), output, cfg)

	if err == nil {
		t.Errorf("Error must not be nil")
		return
	}

	if !strings.Contains(err.Error(), errMsg) {
		t.Errorf("Error message expected to contain %s actual %s", errMsg, err.Error())
	}
}

func TestStepName(t *testing.T) {
	s := Step{}

	if s.Name() != StepName {
		t.Errorf("Unexpected step name expected %s actual %s", StepName, s.Name())
	}
}

func TestDepends(t *testing.T) {
	s := Step{}

	if len(s.Depends()) != 1 && s.Depends()[0] != docker.StepName {
		t.Errorf("Wrong dependency list %v expected %v", s.Depends(), []string{docker.StepName})
	}
}

func TestStep_Rollback(t *testing.T) {
	s := Step{}
	err := s.Rollback(context.Background(), ioutil.Discard, &steps.Config{})

	if err != nil {
		t.Errorf("unexpected error while rollback %v", err)
	}
}

func TestNew(t *testing.T) {
	tpl := template.New("test")
	s := New(tpl)

	if s.script != tpl {
		t.Errorf("Wrong template expected %v actual %v", tpl, s.script)
	}
}

func TestInit(t *testing.T) {
	templatemanager.SetTemplate(StepName, &template.Template{})
	Init()
	templatemanager.DeleteTemplate(StepName)

	s := steps.GetStep(StepName)

	if s == nil {
		t.Error("Step not found")
	}
}

func TestInitPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("recover output must not be nil")
		}
	}()

	Init()

	s := steps.GetStep("not_found.sh.tpl")

	if s == nil {
		t.Error("Step not found")
	}
}

func TestStep_Description(t *testing.T) {
	s := &Step{}

	if desc := s.Description(); desc != "Install EtcD" {
		t.Errorf("Wrong desription expected %s actual %s",
			"Install EtcD", desc)
	}
}
