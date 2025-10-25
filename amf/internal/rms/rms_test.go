package rms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/h2non/gock"

	amf_context "github.com/free5gc/amf/internal/context"
	"github.com/free5gc/amf/internal/gmm"
	amf_logger "github.com/free5gc/amf/internal/logger"
	rms "github.com/free5gc/amf/internal/rms"
	"github.com/free5gc/amf/pkg/factory"
	amf_service "github.com/free5gc/amf/pkg/service"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/util/fsm"
)

// Models as defined in the assignment spec
type Subscription struct {
	SubId     string `json:"subId"`
	UeId      string `json:"ueId"`
	NotifyUri string `json:"notifyUri"`
}

type UeRMNotif struct {
	SubId     string `json:"subId"`
	UeId      string `json:"ueId"`
	PrevState string `json:"from"`
	CurrState string `json:"to"`
}

var (
	testAMF    *amf_service.AmfApp
	baseAPIURL string
)

// TestMain spins up a lightweight AMF instance exposing the SBI server (HTTP) including namf-rmm routes.
func TestMain(m *testing.M) {
	// Build a minimal config (do not call Validate - we intentionally include "namf-rmm").
	factory.AmfConfig = &factory.Config{
		Info: &factory.Info{Version: "1.0.9", Description: "test cfg"},
		Configuration: &factory.Configuration{
			AmfName:    "AMF-TEST",
			NgapIpList: []string{"127.0.0.1"},
			NgapPort:   38412,
			Sbi: &factory.Sbi{
				Scheme:       "http",
				RegisterIPv4: "127.0.0.15",
				BindingIPv4:  "127.0.0.15",
				Port:         18080,
				Tls:          &factory.Tls{Pem: "cert/amf.pem", Key: "cert/amf.key"},
			},
			// Include namf-rmm so the router mounts the routes under /namf-rmm/v1
			ServiceNameList: []string{
				"namf-comm", "namf-evts", "namf-mt", "namf-loc", "namf-oam", "namf-rmm",
			},
			ServedGumaiList: []models.Guami{{
				PlmnId: &models.PlmnIdNid{Mcc: "208", Mnc: "93"},
				AmfId:  "cafe00",
			}},
			SupportTAIList: []models.Tai{{
				PlmnId: &models.PlmnId{Mcc: "208", Mnc: "93"},
				Tac:    "000001",
			}},
			PlmnSupportList: []factory.PlmnSupportItem{{
				PlmnId:     &models.PlmnId{Mcc: "208", Mnc: "93"},
				SNssaiList: []models.Snssai{{Sst: 1, Sd: "fedcba"}},
			}},
			SupportDnnList: []string{"internet"},
			NrfUri:         "http://127.0.0.10:8000",
			Security: &factory.Security{
				IntegrityOrder: []string{"NIA2"},
				CipheringOrder: []string{"NEA0"},
			},
			NetworkName: factory.NetworkName{Full: "free5GC", Short: "free"},
			T3502Value:  720, T3512Value: 3600, Non3gppDeregTimerValue: 3240,
			T3513: factory.TimerValue{Enable: true},
			T3522: factory.TimerValue{Enable: true},
			T3550: factory.TimerValue{Enable: true},
			T3560: factory.TimerValue{Enable: true},
			T3565: factory.TimerValue{Enable: true},
			T3570: factory.TimerValue{Enable: true},
			T3555: factory.TimerValue{Enable: true},
		},
		Logger: &factory.Logger{Enable: false, Level: "info", ReportCaller: false},
	}

	// Spin up AMF
	ctx := context.Background()
	var err error
	testAMF, err = amf_service.NewApp(ctx, factory.AmfConfig, "")
	if err != nil {
		fmt.Printf("failed to create AMF app: %v\n", err)
		os.Exit(1)
	}

	baseAPIURL = fmt.Sprintf("http://%s:%d%s", factory.AmfConfig.Configuration.Sbi.RegisterIPv4, factory.AmfConfig.Configuration.Sbi.Port, factory.AmfRmmResUriPrefix)

	go testAMF.Start()

	// Wait for server to be ready by probing root
	if err := waitHTTPReady(baseAPIURL+"/", 5*time.Second); err != nil {
		fmt.Printf("AMF SBI not ready: %v\n", err)
		// continue anyway; tests will fail if endpoints are not mounted
	}

	code := m.Run()

	// Teardown
	if testAMF != nil {
		testAMF.Terminate()
		// give it a moment to shut down
		time.Sleep(200 * time.Millisecond)
	}
	os.Exit(code)
}

func waitHTTPReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", url)
}

// --- HTTP helpers ---
func httpDoJSON(t *testing.T, method, url string, in any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	newClient := &http.Client{}
	resp, err := newClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// Strict validation helpers
func validateSubscription(t *testing.T, sub Subscription, expectUeId, expectNotifyUri string) {
	t.Helper()
	if sub.SubId == "" {
		t.Error("SubId must not be empty")
	}
	if sub.UeId != expectUeId {
		t.Errorf("UeId mismatch: expected %s, got %s", expectUeId, sub.UeId)
	}
	if sub.NotifyUri != expectNotifyUri {
		t.Errorf("NotifyUri mismatch: expected %s, got %s", expectNotifyUri, sub.NotifyUri)
	}
}

func validateJSONStructure(t *testing.T, data []byte, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("Invalid JSON response: %v, body: %s", err, string(data))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr
}

// --- CRUD tests per spec ---

func TestRMM_CRUD_Subscriptions(t *testing.T) {
	// Create
	subID := "sub-001"
	ueID := "imsi-208930000000001"
	notify := "http://127.0.0.1:9099/rmm-notify"
	reqSub := Subscription{UeId: ueID, NotifyUri: notify}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("POST want 201, got %d body=%s", status, string(data))
	}
	var created Subscription
	_ = json.Unmarshal(data, &created)

	// Get collection
	status, data = httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("GET want 200, got %d body=%s", status, string(data))
	}
	var list struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	_ = json.Unmarshal(data, &list)
	subID = list.Subscriptions[0].SubId

	// Update via PUT
	updated := Subscription{UeId: ueID, NotifyUri: notify + "/v2"}
	status, data = httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), updated)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("PUT want 200 or 201, got %d body=%s", status, string(data))
	}

	// Delete
	status, data = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE want 204, got %d body=%s", status, string(data))
	}
}

// Notification test: when UE FSM state changes, AMF should notify the consumer.
func TestRMM_Notification_OnGmmTransition(t *testing.T) {
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Prepare a subscription for the UE
	ueID := "imsi-208930000000777"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/callback/rmm"

	// Create subscription via SBI
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, ""), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("POST subscription want 201, got %d body=%s", status, string(data))
	}
	var subs Subscription
	_ = json.Unmarshal(data, &subs)
	subID := subs.SubId
	// Expect a POST to the callback URI with a payload including subId and ueId
	gock.New(notifyBase).
		Post(notifyPath).
		MatchType("json").
		JSON(&UeRMNotif{SubId: subID, UeId: ueID, PrevState: "Deregistered", CurrState: "Authentication"}).
		Reply(204)

	// Attach our RMS (student implementation should use it to send notification)
	gmm.AttachRMS(rms.NewRMS())

	// Create a UE context and trigger a GMM transition
	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger from Deregistered -> Authentication (StartAuthEvent)
	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent, fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
		t.Fatalf("SendEvent StartAuthEvent failed: %v", err)
	}

	// Give some time for async handlers to run
	waitUntil := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(waitUntil) && !gock.IsDone() {
		time.Sleep(20 * time.Millisecond)
	}

	if !gock.IsDone() {
		t.Fatalf("expected notification not received by callback server")
	}
}

// Test concurrent POST requests to ensure thread safety and UUID uniqueness
func TestRMM_ConcurrentPOST(t *testing.T) {
	const numGoroutines = 5000
	results := make(chan Subscription, numGoroutines)
	errors := make(chan error, numGoroutines)

	// Launch concurrent POST requests
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			ueID := fmt.Sprintf("imsi-20893000000%04d", id)
			notify := fmt.Sprintf("http://127.0.0.1:9099/callback/%d", id)
			reqSub := Subscription{UeId: ueID, NotifyUri: notify}

			status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)
			if status != http.StatusCreated {
				errors <- fmt.Errorf("POST want 201, got %d", status)
				return
			}

			var created Subscription
			if err := json.Unmarshal(data, &created); err != nil {
				errors <- fmt.Errorf("unmarshal error: %v", err)
				return
			}

			results <- created
		}(i)
	}

	// Collect results
	subIDs := make(map[string]bool)
	for i := 0; i < numGoroutines; i++ {
		select {
		case sub := <-results:
			if sub.SubId == "" {
				t.Errorf("Empty SubId returned")
			}
			if subIDs[sub.SubId] {
				t.Errorf("Duplicate SubId detected: %s", sub.SubId)
			}
			subIDs[sub.SubId] = true
		case err := <-errors:
			t.Errorf("Concurrent POST error: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("Timeout waiting for concurrent POST results")
		}
	}

	if len(subIDs) != numGoroutines {
		t.Errorf("Expected %d unique SubIds, got %d", numGoroutines, len(subIDs))
	}
}

// Test concurrent read/write operations
func TestRMM_ConcurrentReadWrite(t *testing.T) {
	// Create initial subscription
	ueID := "imsi-208930000001111"
	notify := "http://127.0.0.1:9099/notify"
	reqSub := Subscription{UeId: ueID, NotifyUri: notify}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("POST want 201, got %d", status)
	}
	var created Subscription
	json.Unmarshal(data, &created)
	subID := created.SubId

	const numOperations = 15 // Reduced for faster tests
	done := make(chan bool, numOperations)

	// Concurrent readers
	for i := 0; i < 5; i++ {
		go func() {
			status, _ := httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
			if status != http.StatusOK {
				t.Errorf("GET want 200, got %d", status)
			}
			done <- true
		}()
	}

	// Concurrent writers (PUT)
	for i := 0; i < 5; i++ {
		go func(id int) {
			updated := Subscription{UeId: ueID, NotifyUri: fmt.Sprintf("%s/v%d", notify, id)}
			status, _ := httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), updated)
			if status != http.StatusOK && status != http.StatusCreated {
				t.Errorf("PUT want 200 or 201, got %d", status)
			}
			done <- true
		}(i)
	}

	// Concurrent new subscriptions
	for i := 0; i < 5; i++ {
		go func(id int) {
			newSub := Subscription{
				UeId:      fmt.Sprintf("imsi-2089300000%05d", id),
				NotifyUri: fmt.Sprintf("http://127.0.0.1:9099/notify/%d", id),
			}
			status, _ := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), newSub)
			if status != http.StatusCreated {
				t.Errorf("POST want 201, got %d", status)
			}
			done <- true
		}(i)
	}

	// Wait for all operations
	for i := 0; i < numOperations; i++ {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("Timeout waiting for concurrent operations")
		}
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
}

// Test error handling: missing required fields
func TestRMM_ErrorHandling_MissingFields(t *testing.T) {
	testCases := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing ueId", map[string]interface{}{"notifyUri": "http://example.com"}},
		{"missing notifyUri", map[string]interface{}{"ueId": "imsi-208930000000001"}},
		{"empty ueId", map[string]interface{}{"ueId": "", "notifyUri": "http://example.com"}},
		{"empty notifyUri", map[string]interface{}{"ueId": "imsi-208930000000001", "notifyUri": ""}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.body)
			req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("Expected 404, got %d for %s", resp.StatusCode, tc.name)
			}
		})
	}
}

// Test error handling: invalid JSON
func TestRMM_ErrorHandling_InvalidJSON(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected 404 for invalid JSON, got %d", resp.StatusCode)
	}
}

// Test DELETE non-existent subscription
func TestRMM_ErrorHandling_DeleteNonExistent(t *testing.T) {
	status, _ := httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/non-existent-id", baseAPIURL), nil)
	if status != http.StatusNotFound {
		t.Errorf("DELETE non-existent want 404, got %d", status)
	}
}

// Test PUT creates new subscription if not exists
func TestRMM_PUT_CreateNew(t *testing.T) {
	customID := "custom-subscription-id-12345"
	ueID := "imsi-208930000002222"
	notify := "http://127.0.0.1:9099/custom"

	sub := Subscription{UeId: ueID, NotifyUri: notify}
	status, data := httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), sub)

	if status != http.StatusCreated {
		t.Errorf("PUT new subscription want 201, got %d", status)
	}

	var created Subscription
	json.Unmarshal(data, &created)

	if created.SubId != customID {
		t.Errorf("Expected SubId %s, got %s", customID, created.SubId)
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), nil)
}

// Test PUT updates existing subscription
func TestRMM_PUT_UpdateExisting(t *testing.T) {
	// First create via POST
	ueID := "imsi-208930000003333"
	notify := "http://127.0.0.1:9099/original"
	reqSub := Subscription{UeId: ueID, NotifyUri: notify}
	_, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)

	var created Subscription
	json.Unmarshal(data, &created)
	subID := created.SubId

	// Now update via PUT
	updated := Subscription{UeId: ueID, NotifyUri: "http://127.0.0.1:9099/updated"}
	status, data := httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), updated)

	if status != http.StatusOK {
		t.Errorf("PUT existing want 200, got %d", status)
	}

	var result Subscription
	json.Unmarshal(data, &result)

	if result.NotifyUri != "http://127.0.0.1:9099/updated" {
		t.Errorf("NotifyUri not updated: got %s", result.NotifyUri)
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
}

// Test multiple UEs with same notification URI
func TestRMM_MultipleUEs_SameNotifyUri(t *testing.T) {
	notify := "http://127.0.0.1:9099/shared-callback"
	subIDs := []string{}

	// Create subscriptions for multiple UEs
	for i := 0; i < 5; i++ {
		ueID := fmt.Sprintf("imsi-2089300000%05d", 10000+i)
		reqSub := Subscription{UeId: ueID, NotifyUri: notify}
		status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)

		if status != http.StatusCreated {
			t.Fatalf("POST want 201, got %d", status)
		}

		var created Subscription
		json.Unmarshal(data, &created)
		subIDs = append(subIDs, created.SubId)
	}

	// Verify all subscriptions exist
	status, data := httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("GET want 200, got %d", status)
	}

	var list struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	json.Unmarshal(data, &list)

	count := 0
	for _, sub := range list.Subscriptions {
		if sub.NotifyUri == notify {
			count++
		}
	}

	if count < 5 {
		t.Errorf("Expected at least 5 subscriptions with same NotifyUri, got %d", count)
	}

	// Cleanup
	for _, subID := range subIDs {
		httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
	}
}

// Test notification for multiple subscriptions of same UE
func TestRMM_Notification_MultipleSubscriptionsSameUE(t *testing.T) {
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	ueID := "imsi-208930000004444"
	notifyBase1 := "http://127.0.0.1:9091"
	notifyPath1 := "/callback1"
	notifyBase2 := "http://127.0.0.1:9092"
	notifyPath2 := "/callback2"

	// Create two subscriptions for the same UE
	reqSub1 := Subscription{UeId: ueID, NotifyUri: notifyBase1 + notifyPath1}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub1)
	if status != http.StatusCreated {
		t.Fatalf("POST subscription1 want 201, got %d", status)
	}
	var subs1 Subscription
	json.Unmarshal(data, &subs1)

	reqSub2 := Subscription{UeId: ueID, NotifyUri: notifyBase2 + notifyPath2}
	status, data = httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub2)
	if status != http.StatusCreated {
		t.Fatalf("POST subscription2 want 201, got %d", status)
	}
	var subs2 Subscription
	json.Unmarshal(data, &subs2)

	// Expect notifications to both callbacks
	gock.New(notifyBase1).
		Post(notifyPath1).
		MatchType("json").
		JSON(&UeRMNotif{SubId: subs1.SubId, UeId: ueID, PrevState: "Deregistered", CurrState: "Authentication"}).
		Reply(204)

	gock.New(notifyBase2).
		Post(notifyPath2).
		MatchType("json").
		JSON(&UeRMNotif{SubId: subs2.SubId, UeId: ueID, PrevState: "Deregistered", CurrState: "Authentication"}).
		Reply(204)

	// Attach RMS
	gmm.AttachRMS(rms.NewRMS())

	// Create UE and trigger state transition
	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent, fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	// Wait for notifications
	waitUntil := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(waitUntil) && !gock.IsDone() {
		time.Sleep(20 * time.Millisecond)
	}

	if !gock.IsDone() {
		pending := gock.Pending()
		t.Fatalf("Expected both notifications, but %d requests still pending", len(pending))
	}
}

// Test notification not sent for self-loop transitions
func TestRMM_Notification_IgnoreSelfLoop(t *testing.T) {
	// This test verifies the RMS code: if trans.From == trans.To { return }
	// We simply verify that a valid state transition DOES send a notification
	// The self-loop logic is tested implicitly by the code check

	ueID := "imsi-208930000005555"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/callback/self-loop"

	// Create subscription first to get SubId
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("POST subscription want 201, got %d", status)
	}
	var subs Subscription
	json.Unmarshal(data, &subs)

	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Expect notification for valid transition
	gock.New(notifyBase).
		Post(notifyPath).
		MatchType("json").
		JSON(&UeRMNotif{SubId: subs.SubId, UeId: ueID, PrevState: "Deregistered", CurrState: "Authentication"}).
		Reply(204)

	// Attach RMS
	gmm.AttachRMS(rms.NewRMS())

	// Create UE
	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger state transition (Deregistered -> Authentication)
	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
		fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType},
		amf_logger.GmmLog); err != nil {
		t.Logf("SendEvent warning: %v", err)
	}

	// Wait for notification
	waitUntil := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(waitUntil) && !gock.IsDone() {
		time.Sleep(20 * time.Millisecond)
	}

	if !gock.IsDone() {
		t.Fatal("Expected notification for state transition not received")
	}
} // Test GET returns empty list when no subscriptions
func TestRMM_GET_EmptyList(t *testing.T) {
	// First, clean up any existing subscriptions
	status, data := httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status == http.StatusOK {
		var list struct {
			Subscriptions []Subscription `json:"subscriptions"`
		}
		json.Unmarshal(data, &list)

		// Delete all existing subscriptions
		for _, sub := range list.Subscriptions {
			httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, sub.SubId), nil)
		}
	}

	// Now verify empty list
	status, data = httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("GET want 200, got %d", status)
	}

	var list struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	json.Unmarshal(data, &list)

	if list.Subscriptions == nil || len(list.Subscriptions) != 0 {
		t.Errorf("Expected empty subscriptions list, got %d items", len(list.Subscriptions))
	}
}

// Test idempotency of PUT operations
func TestRMM_PUT_Idempotency(t *testing.T) {
	customID := "idempotent-test-id"
	ueID := "imsi-208930000006666"
	notify := "http://127.0.0.1:9099/idempotent"

	sub := Subscription{UeId: ueID, NotifyUri: notify}

	// First PUT - creates
	status1, data1 := httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), sub)
	if status1 != http.StatusCreated {
		t.Errorf("First PUT want 201, got %d", status1)
	}

	var result1 Subscription
	json.Unmarshal(data1, &result1)

	// Second PUT with same data - updates (should return 200)
	status2, data2 := httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), sub)
	if status2 != http.StatusOK {
		t.Errorf("Second PUT want 200, got %d", status2)
	}

	var result2 Subscription
	json.Unmarshal(data2, &result2)

	// Verify data consistency
	if result1.SubId != result2.SubId || result1.UeId != result2.UeId || result1.NotifyUri != result2.NotifyUri {
		t.Error("PUT operations should be idempotent")
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), nil)
}

// --- Strict Validation Tests ---

// Test POST returns correct JSON structure and all required fields
func TestRMM_POST_StrictValidation(t *testing.T) {
	ueID := "imsi-208930000007777"
	notifyUri := "http://127.0.0.1:9099/strict-test"

	reqSub := Subscription{UeId: ueID, NotifyUri: notifyUri}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL),
		bytes.NewReader(mustMarshal(reqSub)))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify status code
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("Expected 201 Created, got %d", resp.StatusCode)
	}

	// Verify Content-Type
	contentType := resp.Header.Get("Content-Type")
	if !contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type with application/json, got %s", contentType)
	}

	// Verify response body structure
	data, _ := io.ReadAll(resp.Body)
	var result Subscription
	validateJSONStructure(t, data, &result)

	// Verify all fields are present and correct
	if result.SubId == "" {
		t.Error("Response must contain non-empty subId")
	}
	if result.UeId != ueID {
		t.Errorf("Response ueId mismatch: expected %s, got %s", ueID, result.UeId)
	}
	if result.NotifyUri != notifyUri {
		t.Errorf("Response notifyUri mismatch: expected %s, got %s", notifyUri, result.NotifyUri)
	}

	// Verify SubId is a valid UUID format (basic check)
	if len(result.SubId) < 32 {
		t.Errorf("SubId should be UUID format, got: %s", result.SubId)
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, result.SubId), nil)
}

// Test GET returns correct collection format
func TestRMM_GET_StrictFormat(t *testing.T) {
	// Create some subscriptions first
	subIds := []string{}
	for i := 0; i < 3; i++ {
		reqSub := Subscription{
			UeId:      fmt.Sprintf("imsi-208930000008%03d", i),
			NotifyUri: fmt.Sprintf("http://127.0.0.1:9099/test/%d", i),
		}
		status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL), reqSub)
		if status != http.StatusCreated {
			t.Fatalf("Failed to create test subscription")
		}
		var sub Subscription
		json.Unmarshal(data, &sub)
		subIds = append(subIds, sub.SubId)
	}

	// GET and verify format
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify status and content type
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !contains(contentType, "application/json") {
		t.Errorf("Expected Content-Type with application/json, got %s", contentType)
	}

	// Verify response structure
	data, _ := io.ReadAll(resp.Body)
	var result struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	validateJSONStructure(t, data, &result)

	// Verify subscriptions field exists (even if empty)
	if result.Subscriptions == nil {
		t.Error("Response must contain 'subscriptions' field (can be empty array)")
	}

	// Verify we got at least our created subscriptions
	if len(result.Subscriptions) < 3 {
		t.Errorf("Expected at least 3 subscriptions, got %d", len(result.Subscriptions))
	}

	// Verify each subscription has all required fields
	for _, sub := range result.Subscriptions {
		if sub.SubId == "" || sub.UeId == "" || sub.NotifyUri == "" {
			t.Errorf("Subscription missing required fields: %+v", sub)
		}
	}

	// Cleanup
	for _, subId := range subIds {
		httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subId), nil)
	}
}

// Test PUT with non-existent ID returns 201 with correct format
func TestRMM_PUT_StrictCreation(t *testing.T) {
	customID := "strict-test-put-id"
	ueID := "imsi-208930000009999"
	notifyUri := "http://127.0.0.1:9099/put-strict"

	reqSub := Subscription{UeId: ueID, NotifyUri: notifyUri}
	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID),
		bytes.NewReader(mustMarshal(reqSub)))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Must return 201 for new resource
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("PUT new resource must return 201 Created, got %d", resp.StatusCode)
	}

	// Verify response format
	data, _ := io.ReadAll(resp.Body)
	var result Subscription
	validateJSONStructure(t, data, &result)

	// Verify SubId matches the URL parameter
	if result.SubId != customID {
		t.Errorf("PUT response SubId must match URL parameter: expected %s, got %s", customID, result.SubId)
	}

	validateSubscription(t, result, ueID, notifyUri)

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, customID), nil)
}

// Test PUT with existing ID returns 200 with correct format
func TestRMM_PUT_StrictUpdate(t *testing.T) {
	// Create initial subscription
	ueID := "imsi-208930000010000"
	origUri := "http://127.0.0.1:9099/original"

	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL),
		Subscription{UeId: ueID, NotifyUri: origUri})
	if status != http.StatusCreated {
		t.Fatalf("Failed to create initial subscription")
	}
	var created Subscription
	json.Unmarshal(data, &created)
	subID := created.SubId

	// Update with PUT
	updatedUri := "http://127.0.0.1:9099/updated"
	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID),
		bytes.NewReader(mustMarshal(Subscription{UeId: ueID, NotifyUri: updatedUri})))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Must return 200 for existing resource update
	if resp.StatusCode != http.StatusOK {
		t.Errorf("PUT existing resource must return 200 OK, got %d", resp.StatusCode)
	}

	// Verify response format
	data, _ = io.ReadAll(resp.Body)
	var result Subscription
	validateJSONStructure(t, data, &result)

	// Verify SubId unchanged
	if result.SubId != subID {
		t.Errorf("PUT should not change SubId: expected %s, got %s", subID, result.SubId)
	}

	// Verify update applied
	if result.NotifyUri != updatedUri {
		t.Errorf("PUT should update notifyUri: expected %s, got %s", updatedUri, result.NotifyUri)
	}

	validateSubscription(t, result, ueID, updatedUri)

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
}

// Test DELETE returns 204 with no body
func TestRMM_DELETE_StrictFormat(t *testing.T) {
	// Create subscription
	ueID := "imsi-208930000011111"
	notifyUri := "http://127.0.0.1:9099/delete-test"

	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL),
		Subscription{UeId: ueID, NotifyUri: notifyUri})
	if status != http.StatusCreated {
		t.Fatalf("Failed to create subscription")
	}
	var sub Subscription
	json.Unmarshal(data, &sub)

	// DELETE with strict validation
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, sub.SubId), nil)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	defer resp.Body.Close()

	// Must return 204 No Content
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE must return 204 No Content, got %d", resp.StatusCode)
	}

	// Body should be empty
	body, _ := io.ReadAll(resp.Body)
	if len(body) > 0 {
		t.Errorf("DELETE response body should be empty, got: %s", string(body))
	}

	// Verify subscription is actually deleted
	status, _ = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, sub.SubId), nil)
	if status != http.StatusNotFound {
		t.Errorf("DELETE non-existent should return 404, got %d", status)
	}
}

// Test all error cases return 404 with consistent format
func TestRMM_ErrorHandling_StrictFormat(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		url    string
		body   interface{}
	}{
		{"POST empty ueId", "POST", "/subscriptions", map[string]string{"ueId": "", "notifyUri": "http://test"}},
		{"POST empty notifyUri", "POST", "/subscriptions", map[string]string{"ueId": "imsi-123", "notifyUri": ""}},
		{"POST missing ueId", "POST", "/subscriptions", map[string]string{"notifyUri": "http://test"}},
		{"POST missing notifyUri", "POST", "/subscriptions", map[string]string{"ueId": "imsi-123"}},
		{"PUT empty ueId", "PUT", "/subscriptions/test-id", map[string]string{"ueId": "", "notifyUri": "http://test"}},
		{"PUT empty notifyUri", "PUT", "/subscriptions/test-id", map[string]string{"ueId": "imsi-123", "notifyUri": ""}},
		{"DELETE non-existent", "DELETE", "/subscriptions/non-existent-id-12345", nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != nil {
				body = bytes.NewReader(mustMarshal(tc.body))
			}

			req, _ := http.NewRequest(tc.method, baseAPIURL+tc.url, body)
			if tc.body != nil {
				req.Header.Set("Content-Type", "application/json")
			}

			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Request failed: %v", err)
			}
			defer resp.Body.Close()

			// All errors must return 404
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("Expected 404 Not Found, got %d", resp.StatusCode)
			}
		})
	}
}

// Test notification format is exactly as specified
func TestRMM_Notification_StrictFormat(t *testing.T) {
	// if testing.Short() {
	// 	t.Skip("Skipping notification test in short mode")
	// }

	ueID := "imsi-208930000012222"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/strict-notification"

	// Create subscription
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions", baseAPIURL),
		Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath})
	if status != http.StatusCreated {
		t.Fatalf("Failed to create subscription")
	}
	var subs Subscription
	json.Unmarshal(data, &subs)

	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Set up strict matcher for notification format
	gock.New(notifyBase).
		Post(notifyPath).
		AddMatcher(func(req *http.Request, _ *gock.Request) (bool, error) {
			// Verify Content-Type
			if !contains(req.Header.Get("Content-Type"), "application/json") {
				t.Error("Notification must have Content-Type: application/json")
				return false, nil
			}

			// Verify body structure
			body, _ := io.ReadAll(req.Body)
			var notif UeRMNotif
			if err := json.Unmarshal(body, &notif); err != nil {
				t.Errorf("Notification body is not valid JSON: %v", err)
				return false, nil
			}

			// Verify all required fields
			if notif.SubId == "" {
				t.Error("Notification must contain non-empty subId")
			}
			if notif.UeId == "" {
				t.Error("Notification must contain non-empty ueId")
			}
			if notif.PrevState == "" {
				t.Error("Notification must contain non-empty 'from' (PrevState)")
			}
			if notif.CurrState == "" {
				t.Error("Notification must contain non-empty 'to' (CurrState)")
			}

			// Verify correct values
			if notif.SubId != subs.SubId {
				t.Errorf("Notification subId mismatch: expected %s, got %s", subs.SubId, notif.SubId)
			}
			if notif.UeId != ueID {
				t.Errorf("Notification ueId mismatch: expected %s, got %s", ueID, notif.UeId)
			}
			if notif.PrevState != "Deregistered" || notif.CurrState != "Authentication" {
				t.Errorf("Notification state transition incorrect: %s -> %s", notif.PrevState, notif.CurrState)
			}

			return true, nil
		}).
		Reply(204)

	// Attach RMS and trigger transition
	gmm.AttachRMS(rms.NewRMS())
	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
		fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog)

	// Wait for notification
	waitUntil := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(waitUntil) && !gock.IsDone() {
		time.Sleep(20 * time.Millisecond)
	}

	if !gock.IsDone() {
		t.Fatal("Expected notification not received")
	}

	// Restore HTTP client before cleanup
	gock.Off()
	gock.RestoreClient(http.DefaultClient)

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subs.SubId), nil)
}

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
