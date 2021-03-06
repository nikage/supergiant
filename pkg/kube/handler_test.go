package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"

	"github.com/supergiant/control/pkg/clouds"
	"github.com/supergiant/control/pkg/message"
	"github.com/supergiant/control/pkg/model"
	"github.com/supergiant/control/pkg/node"
	"github.com/supergiant/control/pkg/profile"
	"github.com/supergiant/control/pkg/proxy"
	"github.com/supergiant/control/pkg/sgerrors"
	"github.com/supergiant/control/pkg/testutils"
	"github.com/supergiant/control/pkg/workflows"
	"github.com/supergiant/control/pkg/workflows/steps"
)

var (
	errFake = errors.New("fake error")

	deployedReleaseInput = `{"chartName":"nginx","namespace":"default","repoName":"fake"}`
	deployedRelease      = &release.Release{
		Name:      "fakeDeployed",
		Namespace: "default",
		Chart: &chart.Chart{
			Metadata: &chart.Metadata{
				Name: "nginx",
			},
		},
		Info: &release.Info{
			Status: &release.Status{
				Code: release.Status_DEPLOYED,
			},
		},
	}
	deployedReleaseInfo = &model.ReleaseInfo{
		Name:      "fakeDeleted",
		Namespace: "default",
		Chart:     "nginx",
		Status:    release.Status_Code_name[int32(release.Status_DEPLOYED)],
	}

	deletedReleaseInput = `{"chartName":"esync","namespace":"kube-system","repoName":"fake"}`
	deletedReleaseInfo  = &model.ReleaseInfo{
		Name:      "fakeDeleted",
		Namespace: "kube-system",
		Chart:     "esync",
		Status:    release.Status_Code_name[int32(release.Status_DELETED)],
	}
)

type kubeServiceMock struct {
	mock.Mock
	rls         *release.Release
	rlsInfo     *model.ReleaseInfo
	rlsInfoList []*model.ReleaseInfo
	rlsErr      error
}

type accServiceMock struct {
	mock.Mock
}

type mockNodeProvisioner struct {
	mock.Mock
}

type bufferCloser struct {
	bytes.Buffer
	err error
}

func (b *bufferCloser) Close() error {
	return b.err
}

const (
	serviceCreate            = "Create"
	serviceGet               = "Get"
	serviceListAll           = "ListAll"
	serviceDelete            = "Delete"
	serviceListKubeResources = "ListKubeResources"
	serviceKubeConfigFor     = "KubeConfigFor"
	serviceGetKubeResources  = "GetKubeResources"
	serviceGetCerts          = "GetCerts"
)

func (m *mockNodeProvisioner) ProvisionNodes(ctx context.Context, nodeProfile []profile.NodeProfile, kube *model.Kube, config *steps.Config) ([]string, error) {
	args := m.Called(ctx, nodeProfile, kube, config)
	val, ok := args.Get(0).([]string)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}

func (m *mockNodeProvisioner) Cancel(clusterID string) error {
	args := m.Called(clusterID)
	val, ok := args.Get(0).(error)
	if !ok {
		return args.Error(0)
	}
	return val
}

func (m *kubeServiceMock) Create(ctx context.Context, k *model.Kube) error {
	args := m.Called(ctx, k)
	val, ok := args.Get(0).(error)
	if !ok {
		return nil
	}
	return val
}
func (m *kubeServiceMock) Get(ctx context.Context, name string) (*model.Kube, error) {
	args := m.Called(ctx, name)
	val, ok := args.Get(0).(*model.Kube)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}
func (m *kubeServiceMock) KubeConfigFor(ctx context.Context, kname, user string) ([]byte, error) {
	args := m.Called(ctx, kname, user)
	val, ok := args.Get(0).([]byte)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}
func (m *kubeServiceMock) ListAll(ctx context.Context) ([]model.Kube, error) {
	args := m.Called(ctx)
	val, ok := args.Get(0).([]model.Kube)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}

func (m *kubeServiceMock) Delete(ctx context.Context, name string) error {
	args := m.Called(ctx, name)
	return args.Error(0)
}

func (m *kubeServiceMock) ListKubeResources(ctx context.Context, kname string) ([]byte, error) {
	args := m.Called(ctx, kname)
	val, ok := args.Get(0).([]byte)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}

func (m *kubeServiceMock) GetKubeResources(ctx context.Context, kname, resource, ns, name string) ([]byte, error) {
	args := m.Called(ctx, kname, resource, ns, name)
	val, ok := args.Get(0).([]byte)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}

func (m *kubeServiceMock) GetCerts(ctx context.Context, kname, cname string) (*Bundle, error) {
	args := m.Called(ctx, kname, cname)
	val, ok := args.Get(0).(*Bundle)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}
func (m *kubeServiceMock) InstallRelease(ctx context.Context, kname string, rls *ReleaseInput) (*release.Release, error) {
	return m.rls, m.rlsErr
}
func (m *kubeServiceMock) ReleaseDetails(ctx context.Context, kname string, rlsName string) (*release.Release, error) {
	return m.rls, m.rlsErr
}
func (m *kubeServiceMock) ListReleases(ctx context.Context, kname, ns, offset string, limit int) ([]*model.ReleaseInfo, error) {
	return m.rlsInfoList, m.rlsErr
}
func (m *kubeServiceMock) DeleteRelease(ctx context.Context, kname, rlsName string, purge bool) (*model.ReleaseInfo, error) {
	return m.rlsInfo, m.rlsErr
}

type mockContainter struct {
	mock.Mock
}

func (m *mockContainter) RegisterProxies(targets []*proxy.Target) error {
	args := m.Called(targets)
	val, ok := args.Get(0).(error)
	if !ok {
		return args.Error(0)
	}
	return val
}

func (m *mockContainter) GetProxies(prefix string) map[string]*proxy.ServiceReverseProxy {
	args := m.Called(prefix)
	val, ok := args.Get(0).(map[string]*proxy.ServiceReverseProxy)
	if !ok {
		return nil
	}
	return val
}

func (m *mockContainter) Shutdown(ctx context.Context) {
	m.Called(ctx)
}

func (a *accServiceMock) Get(ctx context.Context, name string) (*model.CloudAccount, error) {
	args := a.Called(ctx, name)

	val, ok := args.Get(0).(*model.CloudAccount)
	if !ok {
		return nil, args.Error(1)
	}
	return val, args.Error(1)
}

func TestHandler_createKube(t *testing.T) {
	tcs := []struct {
		rawKube []byte

		serviceCreateError error
		serviceGetResp     *model.Kube
		serviceGetError    error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			rawKube:         []byte(`{"name":"invalid_json"",,}`),
			expectedStatus:  http.StatusBadRequest,
			expectedErrCode: sgerrors.InvalidJSON,
		},
		{
			rawKube: []byte(`{"name":"newKube"}`),
			serviceGetResp: &model.Kube{
				Name: "alreadyExists",
			},
			expectedStatus:  http.StatusConflict,
			expectedErrCode: sgerrors.AlreadyExists,
		},
		{ // TC#2
			rawKube:         []byte(`{"name":""}`),
			expectedStatus:  http.StatusBadRequest,
			expectedErrCode: sgerrors.ValidationFailed,
		},
		{ // TC#3
			rawKube:            []byte(`{"name":"fail_to_put"}`),
			serviceCreateError: errors.New("error"),
			expectedStatus:     http.StatusInternalServerError,
			expectedErrCode:    sgerrors.UnknownError,
		},
		{ // TC#4
			rawKube:        []byte(`{"name":"success"}`),
			expectedStatus: http.StatusAccepted,
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		req, err := http.NewRequest(http.MethodPost, "/kubes",
			bytes.NewReader(tc.rawKube))
		require.Equalf(t, nil, err,
			"TC#%d: create request: %v", i+1, err)

		svc.On(serviceCreate, mock.Anything, mock.Anything).
			Return(tc.serviceCreateError)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(tc.serviceGetResp, tc.serviceGetError)

		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}
	}
}

func TestHandler_getKube(t *testing.T) {
	tcs := []struct {
		kubeName string

		serviceKube  *model.Kube
		serviceError error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			kubeName:       "",
			expectedStatus: http.StatusNotFound,
		},
		{ // TC#2
			kubeName:        "service_error",
			serviceError:    errors.New("get error"),
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{ // TC#3
			kubeName:        "not_found",
			serviceError:    sgerrors.ErrNotFound,
			expectedStatus:  http.StatusNotFound,
			expectedErrCode: sgerrors.NotFound,
		},
		{ // TC#4
			kubeName: "success",
			serviceKube: &model.Kube{
				Name: "success",
			},
			expectedStatus: http.StatusOK,
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		// prepare
		req, err := http.NewRequest(http.MethodGet, "/kubes/"+tc.kubeName, nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceGet, mock.Anything, tc.kubeName).Return(tc.serviceKube, tc.serviceError)
		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}

		if tc.serviceKube != nil {
			k := new(model.Kube)
			err = json.NewDecoder(rr.Body).Decode(k)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, k, tc.serviceKube, "TC#%d", i+1)
		}
	}
}

func TestHandler_listKubes(t *testing.T) {
	tcs := []struct {
		serviceKubes []model.Kube
		serviceError error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			serviceError:    errors.New("error"),
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{ // TC#2
			expectedStatus: http.StatusOK,
			serviceKubes: []model.Kube{
				{
					Name: "success",
				},
			},
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		// prepare
		req, err := http.NewRequest(http.MethodGet, "/kubes", nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceListAll, mock.Anything).Return(tc.serviceKubes, tc.serviceError)
		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}

		if tc.serviceKubes != nil {
			kubes := new([]model.Kube)
			err = json.NewDecoder(rr.Body).Decode(kubes)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.serviceKubes, *kubes, "TC#%d", i+1)
		}
	}
}

func TestHandler_deleteKube(t *testing.T) {
	tcs := []struct {
		description string
		kubeName    string

		accountName     string
		getAccountError error
		account         *model.CloudAccount

		kube            *model.Kube
		getKubeError    error
		deleteKubeError error

		expectedStatus int
	}{
		{
			description:    "kube not found",
			kubeName:       "test",
			getKubeError:   sgerrors.ErrNotFound,
			expectedStatus: http.StatusNotFound,
		},
		{
			description: "account not found",
			kubeName:    "service_error",

			accountName:     "test",
			getAccountError: sgerrors.ErrNotFound,
			account:         nil,
			kube: &model.Kube{
				Provider:    clouds.DigitalOcean,
				Name:        "test",
				AccountName: "test",
			},

			expectedStatus: http.StatusNotFound,
		},
		{
			description:     "delete kube err not found",
			kubeName:        "kubeName",
			getAccountError: nil,
			accountName:     "test",
			account: &model.CloudAccount{
				Name:     "test",
				Provider: clouds.DigitalOcean,
			},
			getKubeError: nil,
			kube: &model.Kube{
				Provider:    clouds.DigitalOcean,
				Name:        "test",
				AccountName: "test",
			},
			deleteKubeError: sgerrors.ErrNotFound,
			expectedStatus:  http.StatusAccepted,
		},
		{
			description:     "success",
			kubeName:        "delete kube error",
			getAccountError: nil,
			accountName:     "test",
			account: &model.CloudAccount{
				Name:     "test",
				Provider: clouds.DigitalOcean,
			},
			getKubeError: nil,
			kube: &model.Kube{
				Provider:    clouds.DigitalOcean,
				Name:        "test",
				AccountName: "test",
			},
			deleteKubeError: nil,
			expectedStatus:  http.StatusAccepted,
		},
	}

	for i, tc := range tcs {
		t.Log(tc.description)
		// setup handler
		svc := new(kubeServiceMock)
		accSvc := new(accServiceMock)

		// prepare
		req, err := http.NewRequest(http.MethodDelete, "/kubes/"+tc.kubeName, nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceGet, mock.Anything, tc.kubeName).Return(tc.kube, tc.getKubeError)
		svc.On(serviceDelete, mock.Anything, tc.kubeName).Return(tc.deleteKubeError)
		svc.On(serviceCreate, mock.Anything, mock.Anything).Return(nil)

		accSvc.On(serviceGet, mock.Anything, tc.accountName).Return(tc.account, tc.getAccountError)
		mockRepo := new(testutils.MockStorage)
		mockRepo.On("Put", mock.Anything, mock.Anything,
			mock.Anything, mock.Anything).Return(nil)
		mockRepo.On("Delete", mock.Anything,
			mock.Anything, mock.Anything).Return(nil)
		mockRepo.On("GetAll", mock.Anything,
			mock.Anything).Return([][]byte{}, nil)

		workflows.Init()
		workflows.RegisterWorkFlow(workflows.DigitalOceanDeleteCluster, []steps.Step{})

		rr := httptest.NewRecorder()

		mockProvisioner := new(mockNodeProvisioner)
		mockProvisioner.On("Cancel", mock.Anything).
			Return(nil)

		h := NewHandler(svc, accSvc, mockProvisioner, mockRepo, nil)

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		if tc.expectedStatus != rr.Code {
			t.Errorf("Wrong response code expected %d actual %d",
				tc.expectedStatus, rr.Code)
		}
	}
}

func TestHandler_listResources(t *testing.T) {
	tcs := []struct {
		kubeName string

		serviceResources []byte
		serviceError     error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			kubeName:       "",
			expectedStatus: http.StatusNotFound,
		},
		{ // TC#2
			kubeName:        "service_error",
			serviceError:    errors.New("get error"),
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{ // TC#3
			kubeName:        "not_found",
			serviceError:    sgerrors.ErrNotFound,
			expectedStatus:  http.StatusNotFound,
			expectedErrCode: sgerrors.NotFound,
		},
		{ // TC#4
			kubeName:       "list_resources",
			expectedStatus: http.StatusOK,
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		// prepare
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("/kubes/%s/resources", tc.kubeName), nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceListKubeResources, mock.Anything, tc.kubeName).Return(tc.serviceResources, tc.serviceError)
		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}
	}
}

func TestHandler_getResources(t *testing.T) {
	tcs := []struct {
		kubeName     string
		resourceName string

		serviceResources []byte
		serviceError     error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			kubeName:       "",
			expectedStatus: http.StatusNotFound,
		},
		{ // TC#2
			kubeName:        "service_error",
			resourceName:    "service_error",
			serviceError:    errors.New("get error"),
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{ // TC#3
			kubeName:        "not_found",
			resourceName:    "not_found",
			serviceError:    sgerrors.ErrNotFound,
			expectedStatus:  http.StatusNotFound,
			expectedErrCode: sgerrors.NotFound,
		},
		{ // TC#4
			kubeName:       "list_resources",
			resourceName:   "list_resources",
			expectedStatus: http.StatusOK,
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		// prepare
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("/kubes/%s/resources/%s", tc.kubeName, tc.resourceName), nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceGetKubeResources, mock.Anything, tc.kubeName, mock.Anything, mock.Anything, mock.Anything).
			Return(tc.serviceResources, tc.serviceError)
		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}
	}
}

func TestAddNodeToKube(t *testing.T) {
	testCases := []struct {
		testName       string
		kubeName       string
		kube           *model.Kube
		kubeServiceErr error

		accountName string
		account     *model.CloudAccount
		accountErr  error

		provisionErr error

		expectedCode int
	}{
		{
			"kube not found",
			"test",
			nil,
			sgerrors.ErrNotFound,
			"",
			nil,
			nil,
			nil,
			http.StatusNotFound,
		},
		{
			"account not found",
			"test",
			&model.Kube{
				AccountName: "test",
			},
			nil,
			"test",
			nil,
			sgerrors.ErrNotFound,
			nil,
			http.StatusNotFound,
		},
		{
			"provision not found",
			"test",
			&model.Kube{
				AccountName: "test",
			},
			nil,
			"test",
			&model.CloudAccount{
				Name:     "test",
				Provider: clouds.DigitalOcean,
			},
			nil,
			sgerrors.ErrNotFound,
			http.StatusNotFound,
		},
		{
			"provision success",
			"test",
			&model.Kube{
				AccountName: "test",
				Masters: map[string]*node.Node{
					"": {},
				},
			},
			nil,
			"test",
			&model.CloudAccount{
				Name:     "test",
				Provider: clouds.DigitalOcean,
			},
			nil,
			nil,
			http.StatusAccepted,
		},
	}

	nodeProfile := []profile.NodeProfile{
		{
			"size":  "s-2vcpu-4gb",
			"image": "ubuntu-18-04-x64",
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.testName)
		svc := new(kubeServiceMock)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(testCase.kube, testCase.kubeServiceErr)
		svc.On(serviceCreate, mock.Anything, mock.Anything).
			Return(nil)

		accService := new(accServiceMock)
		accService.On("Get", mock.Anything, mock.Anything).
			Return(testCase.account, testCase.accountErr)

		mockProvisioner := new(mockNodeProvisioner)
		mockProvisioner.On("ProvisionNodes",
			mock.Anything, nodeProfile, testCase.kube, mock.Anything).
			Return(mock.Anything, testCase.provisionErr)
		mockProvisioner.On("Cancel", mock.Anything).
			Return(nil)
		h := NewHandler(svc, accService, mockProvisioner, nil, nil)

		data, _ := json.Marshal(nodeProfile)
		b := bytes.NewBuffer(data)

		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("/kubes/%s/nodes", testCase.kubeName),
			b)
		rec := httptest.NewRecorder()
		router := mux.NewRouter()

		router.HandleFunc("/kubes/{kubeID}/nodes", h.addNode)
		router.ServeHTTP(rec, req)

		if rec.Code != testCase.expectedCode {
			t.Errorf("Wrong error code expected %d actual %d",
				testCase.expectedCode, rec.Code)
		}
	}
}

func TestDeleteNodeFromKube(t *testing.T) {
	testCases := []struct {
		testName string

		nodeName       string
		kubeName       string
		kube           *model.Kube
		kubeServiceErr error

		accountName string
		account     *model.CloudAccount
		accountErr  error

		getWriter    func(string) (io.WriteCloser, error)
		expectedCode int
	}{
		{
			"kube not found",
			"test",
			"test",
			nil,
			sgerrors.ErrNotFound,
			"",
			nil,
			nil,
			nil,
			http.StatusNotFound,
		},
		{
			"get kube unknown error",
			"test",
			"test",
			nil,
			errors.New("unknown"),
			"",
			nil,
			nil,
			nil,
			http.StatusInternalServerError,
		},
		{
			"method not allowed",
			"test",
			"test",
			&model.Kube{
				Masters: map[string]*node.Node{
					"test": {
						Name: "test",
					},
				},
			},
			nil,
			"",
			nil,
			nil,
			nil,
			http.StatusMethodNotAllowed,
		},
		{
			"node not found",
			"test",
			"test",
			&model.Kube{
				Nodes: map[string]*node.Node{
					"test2": {
						Name: "test2",
					},
				},
			},
			nil,
			"",
			nil,
			nil,
			nil,
			http.StatusNotFound,
		},
		{
			"account not found",
			"test",
			"test",
			&model.Kube{
				AccountName: "test",
				Nodes: map[string]*node.Node{
					"test": {
						Name: "test",
					},
				},
			},
			nil,
			"test",
			nil,
			sgerrors.ErrNotFound,
			nil,
			http.StatusNotFound,
		},
		{
			"account unknown error",
			"test",
			"test",
			&model.Kube{
				AccountName: "test",
				Nodes: map[string]*node.Node{
					"test": {
						Name: "test",
					},
				},
			},
			nil,
			"test",
			nil,
			errors.New("account unknown error"),
			nil,
			http.StatusInternalServerError,
		},
		{
			"success",
			"test",
			"test",
			&model.Kube{
				AccountName: "test",
				Nodes: map[string]*node.Node{
					"test": {
						Name: "test",
					},
				},
			},
			nil,
			"test",
			&model.CloudAccount{
				Name:     "test",
				Provider: clouds.DigitalOcean,
				Credentials: map[string]string{
					"publicKey": "publicKey",
				},
			},
			nil,
			func(string) (io.WriteCloser, error) {
				return &bufferCloser{}, nil
			},
			http.StatusAccepted,
		},
	}

	workflows.Init()
	workflows.RegisterWorkFlow(workflows.DigitalOceanDeleteNode, []steps.Step{})

	for _, testCase := range testCases {
		t.Log(testCase.testName)
		svc := new(kubeServiceMock)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(testCase.kube, testCase.kubeServiceErr)
		svc.On(serviceCreate, mock.Anything, testCase.kube).
			Return(mock.Anything)

		accService := new(accServiceMock)
		accService.On("Get", mock.Anything, mock.Anything).
			Return(testCase.account, testCase.accountErr)

		mockRepo := new(testutils.MockStorage)
		mockRepo.On("Put", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(nil)

		mockRepo.On("Delete", mock.Anything, mock.Anything, mock.Anything).
			Return(nil)

		handler := Handler{
			svc:            svc,
			accountService: accService,
			workflowMap: map[clouds.Name]workflows.WorkflowSet{
				clouds.DigitalOcean: {
					DeleteNode: workflows.DigitalOceanDeleteNode},
			},
			getWriter: testCase.getWriter,
			repo:      mockRepo,
		}

		router := mux.NewRouter()
		router.HandleFunc("/{kubeID}/nodes/{nodename}", handler.deleteNode).Methods(http.MethodDelete)

		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("/%s/nodes/%s", testCase.kubeName, testCase.nodeName), nil)
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if rec.Code != testCase.expectedCode {
			t.Errorf("Wrong response code expected %d actual %d", testCase.expectedCode, rec.Code)
		}
	}
}

func TestKubeTasks(t *testing.T) {
	testCases := []struct {
		description string
		repoData    []byte
		repoErr     error

		kubeResp *model.Kube
		kubeErr  error

		err string
	}{
		{
			description: "kube not found",
			kubeResp:    nil,
			kubeErr:     sgerrors.ErrNotFound,
		},
		{
			description: "task not found",
			kubeResp: &model.Kube{
				Tasks: []string{"taskID"},
			},
			kubeErr: nil,
			repoErr: sgerrors.ErrNotFound,
		},
		{
			description: "marshall error",
			kubeResp: &model.Kube{
				Tasks: []string{"taskID"},
			},
			kubeErr:  nil,
			repoData: []byte(`{`),
			repoErr:  nil,
			err:      "unexpected",
		},
		{
			description: "success",
			kubeResp: &model.Kube{
				Tasks: []string{"taskID"},
			},
			kubeErr:  nil,
			repoData: []byte(`{"config": {"clusterId":"test"}}`),
			repoErr:  nil,
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.description)
		svc := new(kubeServiceMock)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(testCase.kubeResp, testCase.kubeErr)

		repo := &testutils.MockStorage{}
		repo.On("Get", mock.Anything, mock.Anything, mock.Anything).
			Return(testCase.repoData, testCase.repoErr)
		h := Handler{
			repo: repo,
			svc:  svc,
		}

		_, err := h.getKubeTasks(context.Background(), "test")

		if err != nil && !strings.Contains(err.Error(), testCase.err) {
			t.Errorf("Wrong error error message expected to have %s actual %s",
				testCase.err, err.Error())
		}
	}
}

func TestDeleteKubeTasks(t *testing.T) {
	testCases := []struct {
		description string
		repoData    []byte
		repoErr     error

		kubeResp *model.Kube
		kubeErr  error

		deleteErr error
	}{
		{
			description: "kube not found",
			kubeErr:     sgerrors.ErrNotFound,
			deleteErr:   sgerrors.ErrNotFound,
		},
		{
			description: "repo not found",
			kubeErr:     nil,
			kubeResp: &model.Kube{
				Tasks: []string{"not_found_id"},
			},
			repoErr: sgerrors.ErrNotFound,
		},
		{
			description: "success",
			kubeErr:     nil,
			kubeResp: &model.Kube{
				Tasks: []string{"1234"},
			},

			repoData: []byte(`{"config": {"clusterId":"test"}}`),
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.description)
		svc := new(kubeServiceMock)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(testCase.kubeResp, testCase.kubeErr)

		repo := &testutils.MockStorage{}
		repo.On("Get", mock.Anything, mock.Anything, mock.Anything).
			Return(testCase.repoData, testCase.repoErr)
		repo.On("Delete", mock.Anything,
			mock.Anything, mock.Anything).
			Return(testCase.deleteErr)
		h := Handler{
			repo: repo,
			svc:  svc,
		}

		err := h.deleteClusterTasks(context.Background(), "test")

		if errors.Cause(err) != testCase.deleteErr {
			t.Errorf("Wrong error expected %v actual %v",
				testCase.deleteErr, err)
		}
	}
}

func TestServiceGetCerts(t *testing.T) {
	testCases := []struct {
		kname string
		cname string

		serviceResp  *Bundle
		serviceErr   error
		expectedCode int
	}{
		{
			kname:        "test",
			cname:        "test",
			serviceResp:  nil,
			serviceErr:   sgerrors.ErrNotFound,
			expectedCode: http.StatusNotFound,
		},
		{
			kname:        "test",
			cname:        "test",
			serviceResp:  nil,
			serviceErr:   errors.New("unknown"),
			expectedCode: http.StatusInternalServerError,
		},

		{
			kname: "test",
			cname: "test",
			serviceResp: &Bundle{
				Cert: []byte(`cert`),
				Key:  []byte(`key`),
			},
			serviceErr:   nil,
			expectedCode: http.StatusOK,
		},
	}

	for _, testCase := range testCases {
		svc := new(kubeServiceMock)
		svc.On(serviceGetCerts, mock.Anything, mock.Anything, mock.Anything).
			Return(testCase.serviceResp, testCase.serviceErr)

		h := Handler{
			svc: svc,
		}

		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("/kubes/%s/certs/%s", testCase.kname, testCase.cname),
			nil)
		rec := httptest.NewRecorder()

		router := mux.NewRouter()
		router.HandleFunc("/kubes/{kubeID}/certs/{cname}", h.getCerts)
		router.ServeHTTP(rec, req)

		if testCase.expectedCode != rec.Code {
			t.Errorf("Wrong response code expected %d actual %d",
				testCase.expectedCode, rec.Code)
		}
	}
}

func TestGetTasks(t *testing.T) {
	testCases := []struct {
		description string
		kubeID      string
		kubeResp    *model.Kube
		kubeErr     error
		repoData    []byte
		repoErr     error

		expectedCode int
	}{
		{
			description:  "kube not found",
			kubeID:       "test",
			kubeErr:      sgerrors.ErrNotFound,
			expectedCode: http.StatusNotFound,
		},
		{
			description: "internal error",
			kubeID:      "test",
			kubeResp: &model.Kube{
				ID:    "test",
				Tasks: []string{"1234"},
			},
			repoData:     []byte(``),
			expectedCode: http.StatusInternalServerError,
		},
		{
			description: "nothing found",
			kubeID:      "test",
			kubeResp: &model.Kube{
				ID:    "test",
				Tasks: []string{"1234"},
			},
			repoErr:      sgerrors.ErrInvalidJson,
			expectedCode: http.StatusNotFound,
		},
		{
			description: "success",
			kubeID:      "test",
			kubeResp: &model.Kube{
				ID:    "test",
				Tasks: []string{"1234"},
			},
			repoData:     []byte(`{"config": {"clusterId":"test"}}`),
			expectedCode: http.StatusOK,
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.description)
		svc := new(kubeServiceMock)
		svc.On(serviceGet, mock.Anything, mock.Anything).
			Return(testCase.kubeResp, testCase.kubeErr)

		repo := &testutils.MockStorage{}
		repo.On("Get", mock.Anything,
			mock.Anything, mock.Anything).
			Return(testCase.repoData, testCase.repoErr)
		h := Handler{
			repo: repo,
			svc:  svc,
		}

		rec := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("/kubes/%s/tasks", testCase.kubeID),
			nil)

		router := mux.NewRouter()
		router.HandleFunc("/kubes/{kubeID}/tasks", h.getTasks)
		router.ServeHTTP(rec, req)

		if testCase.expectedCode != rec.Code {
			t.Errorf("Wrong response code expected %d actual %d",
				testCase.expectedCode, rec.Code)
		}
	}
}

func TestHandler_installRelease(t *testing.T) {
	tcs := []struct {
		rlsInp string

		kubeSvc *kubeServiceMock

		expectedRls     *release.Release
		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{
			rlsInp: "{{}",
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusBadRequest,
			expectedErrCode: sgerrors.InvalidJSON,
		},
		{
			rlsInp: "{}",
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusBadRequest,
			expectedErrCode: sgerrors.ValidationFailed,
		},
		{
			rlsInp: deployedReleaseInput,
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{
			rlsInp: deployedReleaseInput,
			kubeSvc: &kubeServiceMock{
				rls: deployedRelease,
			},
			expectedStatus: http.StatusOK,
			expectedRls:    deployedRelease,
		},
	}

	for i, tc := range tcs {
		// setup handler
		h := &Handler{svc: tc.kubeSvc}

		router := mux.NewRouter()
		h.Register(router)

		// prepare
		req, err := http.NewRequest(
			http.MethodPost,
			"/kubes/fake/releases",
			strings.NewReader(tc.rlsInp))
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		w := httptest.NewRecorder()

		// run
		router.ServeHTTP(w, req)

		// check
		require.Equalf(t, tc.expectedStatus, w.Code, "TC#%d: check status code", i+1)

		if w.Code == http.StatusOK {
			rlsInfo := &release.Release{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(rlsInfo), "TC#%d: decode chart", i+1)

			require.Equalf(t, tc.expectedRls, rlsInfo, "TC#%d: check release", i+1)
		} else {
			apiErr := &message.Message{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(apiErr), "TC#%d: decode message", i+1)

			require.Equalf(t, tc.expectedErrCode, apiErr.ErrorCode, "TC#%d: check error code", i+1)
		}
	}
}

func TestHandler_getRelease(t *testing.T) {
	tcs := []struct {
		kubeSvc *kubeServiceMock

		expectedRls     *release.Release
		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{
			kubeSvc: &kubeServiceMock{
				rls: deployedRelease,
			},
			expectedStatus: http.StatusOK,
			expectedRls:    deployedRelease,
		},
	}

	for i, tc := range tcs {
		// setup handler
		h := &Handler{svc: tc.kubeSvc}

		router := mux.NewRouter()
		h.Register(router)

		// prepare
		req, err := http.NewRequest(
			http.MethodGet,
			"/kubes/fake/releases/releaseName",
			nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		w := httptest.NewRecorder()

		// run
		router.ServeHTTP(w, req)

		// check
		require.Equalf(t, tc.expectedStatus, w.Code, "TC#%d: check status code", i+1)

		if w.Code == http.StatusOK {
			rlsInfo := &release.Release{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(rlsInfo), "TC#%d: decode chart", i+1)

			require.Equalf(t, tc.expectedRls, rlsInfo, "TC#%d: check release", i+1)
		} else {
			apiErr := &message.Message{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(apiErr), "TC#%d: decode message", i+1)

			require.Equalf(t, tc.expectedErrCode, apiErr.ErrorCode, "TC#%d: check error code", i+1)
		}
	}
}

func TestHandler_listReleases(t *testing.T) {
	tcs := []struct {
		kubeSvc *kubeServiceMock

		expectedRlsInfoList []*model.ReleaseInfo
		expectedStatus      int
		expectedErrCode     sgerrors.ErrorCode
	}{
		{
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{
			kubeSvc: &kubeServiceMock{
				rlsInfoList: []*model.ReleaseInfo{deployedReleaseInfo},
			},
			expectedStatus:      http.StatusOK,
			expectedRlsInfoList: []*model.ReleaseInfo{deployedReleaseInfo},
		},
	}

	for i, tc := range tcs {
		// setup handler
		h := &Handler{svc: tc.kubeSvc}

		router := mux.NewRouter()
		h.Register(router)

		// prepare
		req, err := http.NewRequest(
			http.MethodGet,
			"/kubes/fake/releases",
			nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		w := httptest.NewRecorder()

		// run
		router.ServeHTTP(w, req)

		// check
		require.Equalf(t, tc.expectedStatus, w.Code, "TC#%d: check status code", i+1)

		if w.Code == http.StatusOK {
			rlsInfoList := []*model.ReleaseInfo{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(&rlsInfoList), "TC#%d: decode release list", i+1)

			require.Equalf(t, tc.expectedRlsInfoList, rlsInfoList, "TC#%d: check release", i+1)
		} else {
			apiErr := &message.Message{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(apiErr), "TC#%d: decode message", i+1)

			require.Equalf(t, tc.expectedErrCode, apiErr.ErrorCode, "TC#%d: check error code", i+1)
		}
	}
}

func TestHandler_getKubeconfig(t *testing.T) {
	tcs := []struct {
		kubeID   string
		userName string

		serviceResources []byte
		serviceError     error

		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{ // TC#1
			kubeID:         "",
			expectedStatus: http.StatusNotFound,
		},
		{ // TC#2
			kubeID:         "cluster1",
			expectedStatus: http.StatusNotFound,
		},
		{ // TC#2
			kubeID:          "service_error",
			userName:        "uname",
			serviceError:    errors.New("get error"),
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{ // TC#3
			kubeID:          "not_found",
			userName:        "uname",
			serviceError:    sgerrors.ErrNotFound,
			expectedStatus:  http.StatusNotFound,
			expectedErrCode: sgerrors.NotFound,
		},
		{ // TC#4
			kubeID:         "kubeconfig",
			userName:       "uname",
			expectedStatus: http.StatusOK,
		},
	}

	for i, tc := range tcs {
		// setup handler
		svc := new(kubeServiceMock)
		h := NewHandler(svc, nil, nil, nil, nil)

		// prepare
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("/kubes/%s/users/%s/kubeconfig", tc.kubeID, tc.userName), nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		svc.On(serviceKubeConfigFor, mock.Anything, tc.kubeID, tc.userName).Return(tc.serviceResources, tc.serviceError)
		rr := httptest.NewRecorder()

		router := mux.NewRouter().SkipClean(true)
		h.Register(router)

		// run
		router.ServeHTTP(rr, req)

		// check
		require.Equalf(t, tc.expectedStatus, rr.Code, "TC#%d", i+1)

		if tc.expectedErrCode != sgerrors.ErrorCode(0) {
			m := new(message.Message)
			err = json.NewDecoder(rr.Body).Decode(m)
			require.Equalf(t, nil, err, "TC#%d", i+1)

			require.Equalf(t, tc.expectedErrCode, m.ErrorCode, "TC#%d", i+1)
		}
	}
}

func TestHandler_deleteRelease(t *testing.T) {
	tcs := []struct {
		kubeSvc *kubeServiceMock

		expectedRlsInfo *model.ReleaseInfo
		expectedStatus  int
		expectedErrCode sgerrors.ErrorCode
	}{
		{
			kubeSvc: &kubeServiceMock{
				rlsErr: errFake,
			},
			expectedStatus:  http.StatusInternalServerError,
			expectedErrCode: sgerrors.UnknownError,
		},
		{
			kubeSvc: &kubeServiceMock{
				rlsInfo: deletedReleaseInfo,
			},
			expectedStatus:  http.StatusOK,
			expectedRlsInfo: deletedReleaseInfo,
		},
	}

	for i, tc := range tcs {
		// setup handler
		h := &Handler{svc: tc.kubeSvc}

		router := mux.NewRouter()
		h.Register(router)

		// prepare
		req, err := http.NewRequest(
			http.MethodDelete,
			"/kubes/fake/releases/releaseName",
			nil)
		require.Equalf(t, nil, err, "TC#%d: create request: %v", i+1, err)

		w := httptest.NewRecorder()

		// run
		router.ServeHTTP(w, req)

		// check
		require.Equalf(t, tc.expectedStatus, w.Code, "TC#%d: check status code", i+1)

		if w.Code == http.StatusOK {
			rlsInfoList := &model.ReleaseInfo{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(rlsInfoList), "TC#%d: decode release info", i+1)

			require.Equalf(t, tc.expectedRlsInfo, rlsInfoList, "TC#%d: check release", i+1)
		} else {
			apiErr := &message.Message{}
			require.Nilf(t, json.NewDecoder(w.Body).Decode(apiErr), "TC#%d: decode message", i+1)

			require.Equalf(t, tc.expectedErrCode, apiErr.ErrorCode, "TC#%d: check error code", i+1)
		}
	}
}

func TestGetClusterMetrics(t *testing.T) {
	testCases := []struct {
		kubeServiceGetResp  *model.Kube
		kubeServiceGetError error
		getMetrics          func(string, *model.Kube) (*MetricResponse, error)
		expectedCode        int
	}{
		{
			kubeServiceGetError: sgerrors.ErrNotFound,
			expectedCode:        http.StatusNotFound,
		},
		{
			kubeServiceGetError: errors.New("unknown error"),
			expectedCode:        http.StatusInternalServerError,
		},
		{
			kubeServiceGetResp: &model.Kube{
				Name: "test",
				Masters: map[string]*node.Node{
					"master-1": {
						Name:     "master-1",
						PublicIp: "10.20.30.40",
					},
				},
			},
			kubeServiceGetError: nil,
			getMetrics: func(string, *model.Kube) (*MetricResponse, error) {
				return nil, sgerrors.ErrInvalidJson
			},
			expectedCode: http.StatusInternalServerError,
		},
		{
			kubeServiceGetResp: &model.Kube{
				Name: "test",
				Masters: map[string]*node.Node{
					"master-1": {
						Name:     "master-1",
						PublicIp: "10.20.30.40",
					},
				},
			},
			kubeServiceGetError: nil,
			getMetrics: func(string, *model.Kube) (*MetricResponse, error) {
				return &MetricResponse{
					Data: struct {
						ResultType string `json:"resultType"`
						Result     []struct {
							Metric map[string]string `json:"metric"`
							Value  []interface{}     `json:"value"`
						} `json:"result"`
					}{
						ResultType: "metric",
						Result: []struct {
							Metric map[string]string `json:"metric"`
							Value  []interface{}     `json:"value"`
						}{
							{

								Value: []interface{}{"cpu", 0.42},
							},
							{

								Value: []interface{}{"memory", 0.65},
							},
						},
					},
				}, nil
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, testCase := range testCases {
		svc := new(kubeServiceMock)
		svc.On("Get", mock.Anything, mock.Anything).
			Return(testCase.kubeServiceGetResp, testCase.kubeServiceGetError)

		handler := Handler{
			svc:        svc,
			getMetrics: testCase.getMetrics,
		}

		rec := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("/kubes/%s/metrics", "test"), nil)

		router := mux.NewRouter().SkipClean(true)
		handler.Register(router)

		// run
		router.ServeHTTP(rec, req)

		if rec.Code != testCase.expectedCode {
			t.Errorf("Wrong response code expected %d actual %d",
				testCase.expectedCode, rec.Code)
		}
	}
}

func TestGetNodesMetrics(t *testing.T) {
	expectedNodeCount := 3
	testCases := []struct {
		kubeServiceGetResp  *model.Kube
		kubeServiceGetError error
		getMetrics          func(string, *model.Kube) (*MetricResponse, error)
		expectedCode        int
	}{
		{
			kubeServiceGetError: sgerrors.ErrNotFound,
			expectedCode:        http.StatusNotFound,
		},
		{
			kubeServiceGetError: errors.New("unknown error"),
			expectedCode:        http.StatusInternalServerError,
		},
		{
			kubeServiceGetResp: &model.Kube{
				Name: "test",
				Masters: map[string]*node.Node{
					"master-1": {
						Name:     "master-1",
						PublicIp: "10.20.30.40",
					},
				},
			},
			kubeServiceGetError: nil,
			getMetrics: func(string, *model.Kube) (*MetricResponse, error) {
				return nil, sgerrors.ErrInvalidJson
			},
			expectedCode: http.StatusInternalServerError,
		},
		{
			kubeServiceGetResp: &model.Kube{
				Name: "test",
				Masters: map[string]*node.Node{
					"master-1": {
						Name:     "master-1",
						PublicIp: "10.20.30.40",
					},
				},
			},
			kubeServiceGetError: nil,
			getMetrics: func(string, *model.Kube) (*MetricResponse, error) {
				return &MetricResponse{
					Data: struct {
						ResultType string `json:"resultType"`
						Result     []struct {
							Metric map[string]string `json:"metric"`
							Value  []interface{}     `json:"value"`
						} `json:"result"`
					}{
						ResultType: "metric",
						Result: []struct {
							Metric map[string]string `json:"metric"`
							Value  []interface{}     `json:"value"`
						}{
							{
								Metric: map[string]string{
									"node": "node-1",
								},
								Value: []interface{}{"memory", 0.42},
							},
							{

								Metric: map[string]string{
									"node": "node-2",
								},
								Value: []interface{}{"memory", 0.54},
							},
							{

								Metric: map[string]string{
									"node": "master-1",
								},
								Value: []interface{}{"memory", 0.77},
							},
							{
								Metric: map[string]string{
									"node": "node-1",
								},
								Value: []interface{}{"cpu", 0.21},
							},
							{

								Metric: map[string]string{
									"node": "node-2",
								},
								Value: []interface{}{"cpu", 0.35},
							},
							{

								Metric: map[string]string{
									"node": "master-1",
								},
								Value: []interface{}{"cpu", 0.69},
							},
						},
					},
				}, nil
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, testCase := range testCases {
		svc := new(kubeServiceMock)
		svc.On("Get", mock.Anything, mock.Anything).
			Return(testCase.kubeServiceGetResp, testCase.kubeServiceGetError)

		handler := Handler{
			svc:        svc,
			getMetrics: testCase.getMetrics,
		}

		rec := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("/kubes/%s/nodes/metrics", "test"), nil)

		router := mux.NewRouter().SkipClean(true)
		handler.Register(router)

		// run
		router.ServeHTTP(rec, req)

		if rec.Code != testCase.expectedCode {
			t.Errorf("Wrong response code expected %d actual %d",
				testCase.expectedCode, rec.Code)
		}

		if testCase.expectedCode == http.StatusOK {
			resp := map[string]interface{}{}

			err := json.NewDecoder(rec.Body).Decode(&resp)

			if err != nil {
				t.Errorf("Unexpected error %v", err)
			}

			if len(resp) != expectedNodeCount {
				t.Errorf("Unexpected count of nodes expected %d actual %d",
					expectedNodeCount, len(resp))
			}
		}
	}
}

func TestGetServices(t *testing.T) {
	testCases := []struct {
		description string

		getKubeErr error
		getKube    *model.Kube

		getServicesErr error
		k8sServices    *core.ServiceList

		registerProxiesErr error
		getProxies         map[string]*proxy.ServiceReverseProxy

		expectedCode int
	}{
		{
			description:  "kube not found",
			getKubeErr:   sgerrors.ErrNotFound,
			expectedCode: http.StatusNotFound,
		},
		{
			description:  "kube internal error",
			getKubeErr:   errors.New("unknown"),
			expectedCode: http.StatusInternalServerError,
		},
		{
			description: "no master found",
			getKube: &model.Kube{
				ID:      "1234",
				Masters: map[string]*node.Node{},
			},
			expectedCode: http.StatusNotFound,
		},
		{
			description: "get services error",
			getKube: &model.Kube{
				ID: "1234",
				Masters: map[string]*node.Node{
					"key": {
						ID: "key",
					},
				},
			},
			getServicesErr: errors.New("error"),
			expectedCode:   http.StatusInternalServerError,
		},
		{
			description: "register proxy error",

			getKube: &model.Kube{
				ID: "1234",
				Masters: map[string]*node.Node{
					"key": {
						ID: "key",
					},
				},
			},
			k8sServices:        &core.ServiceList{},
			registerProxiesErr: errors.New("error"),
			expectedCode:       http.StatusInternalServerError,
		},
		{
			description: "success 1",

			getKube: &model.Kube{
				ID: "1234",
				Masters: map[string]*node.Node{
					"key": {
						ID: "key",
					},
				},
			},
			k8sServices: &core.ServiceList{
				Items: []core.Service{
					{
						ObjectMeta: meta.ObjectMeta{
							Labels: map[string]string{
								clusterService: "false",
							},
						},
						Spec: core.ServiceSpec{
							Ports: []core.ServicePort{
								{
									Name:     "http",
									Protocol: "TCP",
								},
							},
						},
					},
				},
			},
			getProxies: map[string]*proxy.ServiceReverseProxy{
				"kubeID": {
					ServingBase: "http:/10.20.30.40:9090",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			description: "success 2",

			getKube: &model.Kube{
				ID: "1234",
				Masters: map[string]*node.Node{
					"key": {
						ID: "key",
					},
				},
			},
			k8sServices: &core.ServiceList{
				Items: []core.Service{
					{
						ObjectMeta: meta.ObjectMeta{
							Labels: map[string]string{
								clusterService: "true",
							},
						},
						Spec: core.ServiceSpec{
							Ports: []core.ServicePort{
								{
									Name:     "http",
									Protocol: "TCP",
								},
							},
						},
					},
				},
			},
			getProxies: map[string]*proxy.ServiceReverseProxy{
				"kubeID": {
					ServingBase: "http:/10.20.30.40:9090",
				},
			},
			expectedCode: http.StatusOK,
		},
		{
			description: "success 3",

			getKube: &model.Kube{
				ID: "1234",
				Masters: map[string]*node.Node{
					"key": {
						ID: "key",
					},
				},
			},
			k8sServices: &core.ServiceList{
				Items: []core.Service{
					{
						ObjectMeta: meta.ObjectMeta{
							Labels: map[string]string{
								clusterService: "true",
							},
						},
						Spec: core.ServiceSpec{
							Ports: []core.ServicePort{
								{
									Name:     "other",
									Protocol: "unknown",
								},
								{
									Name:     "http",
									Protocol: "TCP",
								},
							},
						},
					},
				},
			},
			getProxies: map[string]*proxy.ServiceReverseProxy{
				"kubeID": {
					ServingBase: "http:/10.20.30.40:9090",
				},
			},
			expectedCode: http.StatusOK,
		},
	}

	for _, testCase := range testCases {
		t.Log(testCase.description)
		kubeSvc := &kubeServiceMock{}
		kubeSvc.On("Get", mock.Anything, mock.Anything).
			Return(testCase.getKube, testCase.getKubeErr)
		mockProxies := &mockContainter{}
		mockProxies.On("RegisterProxies",
			mock.Anything).Return(testCase.registerProxiesErr)
		mockProxies.On("GetProxies",
			mock.Anything).Return(testCase.getProxies)
		getSvc := func(*model.Kube, string, string) (*core.ServiceList, error) {
			return testCase.k8sServices, testCase.getServicesErr
		}

		handler := &Handler{
			getK8sServices: getSvc,
			svc:            kubeSvc,
			proxies:        mockProxies,
		}

		rec := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet,
			"/kubes/kubeID/services", nil)

		router := mux.NewRouter().SkipClean(true)
		handler.Register(router)

		router.ServeHTTP(rec, req)

		if rec.Code != testCase.expectedCode {
			t.Errorf("Wrong response code expected "+
				"%d actual %d", testCase.expectedCode, rec.Code)
		}
	}
}
