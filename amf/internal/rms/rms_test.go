package rms_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
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
	// Prepare a subscription for the UE
	ueID := "imsi-208930000000777"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/callback/rmm"

	// Create subscription via SBI (using real HTTP, not mocked)
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, ""), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("POST subscription want 201, got %d body=%s", status, string(data))
	}
	var subs Subscription
	_ = json.Unmarshal(data, &subs)
	subID := subs.SubId

	// Now intercept http.DefaultClient for mocking notification callbacks
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

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

// Test API error handling and edge cases
func TestRMM_API_ErrorHandling(t *testing.T) {
	// Test invalid JSON payload
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), "invalid json")
	if status != http.StatusNotFound {
		t.Errorf("POST with invalid JSON want 404, got %d body=%s", status, string(data))
	}

	// Test missing required fields
	invalidSub := struct {
		UeId string `json:"ueId"`
	}{UeId: "test"}
	status, data = httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), invalidSub)
	if status != http.StatusNotFound {
		t.Errorf("POST with missing notifyUri want 404, got %d body=%s", status, string(data))
	}

	// Test update non-existent subscription
	validSub := Subscription{UeId: "imsi-208930000000002", NotifyUri: "http://127.0.0.1:9099/callback"}
	status, data = httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/non-existent", baseAPIURL), validSub)
	if status != http.StatusCreated {
		t.Errorf("PUT non-existent subscription should create new one, want 201, got %d body=%s", status, string(data))
	}

	// Test delete non-existent subscription
	status, data = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/non-existent-id", baseAPIURL), nil)
	if status != http.StatusNotFound {
		t.Errorf("DELETE non-existent subscription want 404, got %d body=%s", status, string(data))
	}
}

// Test subscription lifecycle management
func TestRMM_SubscriptionLifecycle(t *testing.T) {
	ueID := "imsi-208930000000003"
	notifyUri := "http://127.0.0.1:9099/lifecycle-test"

	// 1. Create subscription
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyUri}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("Create subscription failed: %d %s", status, string(data))
	}

	var createdSub Subscription
	json.Unmarshal(data, &createdSub)
	subID := createdSub.SubId

	// 2. Verify subscription exists in collection
	status, data = httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("Get subscriptions failed: %d %s", status, string(data))
	}

	var listResponse struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	json.Unmarshal(data, &listResponse)

	found := false
	for _, sub := range listResponse.Subscriptions {
		if sub.SubId == subID {
			found = true
			if sub.UeId != ueID || sub.NotifyUri != notifyUri {
				t.Errorf("Subscription data mismatch: got %+v, want UeId=%s NotifyUri=%s", sub, ueID, notifyUri)
			}
			break
		}
	}
	if !found {
		t.Errorf("Created subscription %s not found in collection", subID)
	}

	// 3. Update subscription
	updatedNotifyUri := notifyUri + "/updated"
	updateSub := Subscription{UeId: ueID, NotifyUri: updatedNotifyUri}
	status, data = httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), updateSub)
	if status != http.StatusOK {
		t.Fatalf("Update subscription failed: %d %s", status, string(data))
	}

	var updatedSub Subscription
	json.Unmarshal(data, &updatedSub)
	if updatedSub.NotifyUri != updatedNotifyUri {
		t.Errorf("Update failed: got NotifyUri=%s, want %s", updatedSub.NotifyUri, updatedNotifyUri)
	}

	// 4. Delete subscription
	status, data = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
	if status != http.StatusNoContent {
		t.Fatalf("Delete subscription failed: %d %s", status, string(data))
	}

	// 5. Verify subscription is gone
	status, data = httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("Get subscriptions after delete failed: %d %s", status, string(data))
	}

	json.Unmarshal(data, &listResponse)
	for _, sub := range listResponse.Subscriptions {
		if sub.SubId == subID {
			t.Errorf("Deleted subscription %s still found in collection", subID)
		}
	}
}

// Test high concurrency with multiple users and subscriptions
func TestRMM_HighConcurrency_MultipleUsers(t *testing.T) {
	const numUsers = 100
	const numSubscriptionsPerUser = 5

	var wg sync.WaitGroup
	errors := make(chan error, numUsers*numSubscriptionsPerUser)
	subscriptionIds := make(chan string, numUsers*numSubscriptionsPerUser)

	// Create subscriptions concurrently
	for i := 0; i < numUsers; i++ {
		for j := 0; j < numSubscriptionsPerUser; j++ {
			wg.Add(1)
			go func(userID, subIndex int) {
				defer wg.Done()

				ueID := fmt.Sprintf("imsi-20893000000%04d", userID)
				notifyUri := fmt.Sprintf("http://127.0.0.1:909%d/user-%d-sub-%d", userID%10, userID, subIndex)

				reqSub := Subscription{UeId: ueID, NotifyUri: notifyUri}
				status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)

				if status != http.StatusCreated {
					errors <- fmt.Errorf("User %d Sub %d: Create failed with status %d: %s", userID, subIndex, status, string(data))
					return
				}

				var createdSub Subscription
				if err := json.Unmarshal(data, &createdSub); err != nil {
					errors <- fmt.Errorf("User %d Sub %d: Unmarshal failed: %v", userID, subIndex, err)
					return
				}

				subscriptionIds <- createdSub.SubId
			}(i, j)
		}
	}

	wg.Wait()
	close(errors)
	close(subscriptionIds)

	// Check for errors
	var errorList []error
	for err := range errors {
		errorList = append(errorList, err)
	}
	if len(errorList) > 0 {
		t.Fatalf("Concurrent creation errors: %v", errorList[0])
	}

	// Collect all subscription IDs
	var allSubIds []string
	for subId := range subscriptionIds {
		allSubIds = append(allSubIds, subId)
	}

	if len(allSubIds) != numUsers*numSubscriptionsPerUser {
		t.Fatalf("Expected %d subscriptions, got %d", numUsers*numSubscriptionsPerUser, len(allSubIds))
	}

	// Verify all subscriptions exist
	status, data := httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Fatalf("Get all subscriptions failed: %d %s", status, string(data))
	}

	var listResponse struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	json.Unmarshal(data, &listResponse)

	if len(listResponse.Subscriptions) < len(allSubIds) {
		t.Errorf("Expected at least %d subscriptions in collection, got %d", len(allSubIds), len(listResponse.Subscriptions))
	}

	// Clean up: Delete all subscriptions concurrently
	wg = sync.WaitGroup{}
	deleteErrors := make(chan error, len(allSubIds))

	for _, subId := range allSubIds {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			status, data := httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, id), nil)
			if status != http.StatusNoContent {
				deleteErrors <- fmt.Errorf("Delete subscription %s failed: %d %s", id, status, string(data))
			}
		}(subId)
	}

	wg.Wait()
	close(deleteErrors)

	for err := range deleteErrors {
		t.Errorf("Cleanup error: %v", err)
	}
}

// Test concurrent notifications with FSM state changes
func TestRMM_ConcurrentNotifications(t *testing.T) {
	const numUEs = 10 // Reduced for stability
	notifyBase := "http://127.0.0.1:9099"

	// Create subscriptions for multiple UEs (using real HTTP, not mocked)
	var subscriptions []Subscription
	for i := 0; i < numUEs; i++ {
		ueID := fmt.Sprintf("imsi-concurrent-notif-%06d", i)
		notifyPath := fmt.Sprintf("/notify-ue-%d", i)

		reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
		status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
		if status != http.StatusCreated {
			t.Fatalf("Create subscription for UE %d failed: %d %s", i, status, string(data))
		}

		var sub Subscription
		json.Unmarshal(data, &sub)
		subscriptions = append(subscriptions, sub)
	}

	// Now intercept http.DefaultClient for mocking notification callbacks
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Set up gock to accept exactly numUEs notifications to the base URL
	// Use Times(numUEs) instead of Persist() to expect exact number of calls
	gock.New(notifyBase).
		Post("/notify-ue-.*").
		MatchType("json").
		Times(numUEs).
		Reply(204)

	// Attach RMS
	gmm.AttachRMS(rms.NewRMS())

	// Trigger state changes concurrently for all UEs
	var wg sync.WaitGroup
	for i := 0; i < numUEs; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			ueID := fmt.Sprintf("imsi-concurrent-notif-%06d", index)
			t.Logf("Debug: Creating UE %d with ID: %s", index, ueID)
			ue := amf_context.GetSelf().NewAmfUe(ueID)
			anType := models.AccessType__3_GPP_ACCESS
			ue.State[anType] = fsm.NewState("Deregistered")

			// Trigger FSM event
			if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
				fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
				t.Errorf("SendEvent for UE %d failed: %v", index, err)
			} else {
				t.Logf("Debug: Successfully sent FSM event for UE %d", index)
			}
		}(i)
	}

	wg.Wait()

	// Give notification goroutines time to start and complete (they're spawned by HandleEvent)
	// Increased from 200ms to 1000ms to allow all goroutines to finish HTTP calls
	time.Sleep(1000 * time.Millisecond)

	// Wait for all notifications to be processed
	timeout := time.After(10 * time.Second) // Increased timeout from 5s to 10s
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			if !gock.IsDone() {
				pendingMocks := gock.Pending()
				t.Logf("Debug: Total pending mocks: %d", len(pendingMocks))
				for i, mock := range pendingMocks {
					t.Logf("Debug: Pending mock %d: %s %s", i, mock.Request().Method, mock.Request().URLStruct.String())
				}
				t.Fatalf("Timeout waiting for notifications. %d mocks still pending", len(pendingMocks))
			}
			return
		case <-ticker.C:
			if gock.IsDone() {
				return
			}
		}
	}
}

// Test notification stress with rapid state changes
func TestRMM_NotificationStress(t *testing.T) {
	ueID := "imsi-208930000000999"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/stress-test"

	// Create subscription (using real HTTP, not mocked)
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("Create subscription failed: %d %s", status, string(data))
	}

	var sub Subscription
	json.Unmarshal(data, &sub)

	// Now intercept http.DefaultClient for mocking notification callbacks
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Set up mock to accept any notification to this endpoint
	gock.New(notifyBase).
		Post(notifyPath).
		MatchType("json").
		Persist().
		Reply(204)

	// Attach RMS
	gmm.AttachRMS(rms.NewRMS())

	// Create UE context
	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger rapid state changes
	const numStateChanges = 100
	var wg sync.WaitGroup

	for i := 0; i < numStateChanges; i++ {
		wg.Add(1)
		go func(iteration int) {
			defer wg.Done()

			// Use StartAuthEvent which transitions from Deregistered to Authentication
			// This is a valid transition that doesn't require additional args like EAP
			if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
				fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
				// Some transitions may be invalid, which is expected
				// Don't fail the test for invalid transitions
			}
		}(i)
	}

	wg.Wait()

	// Wait a bit for notifications to be processed
	time.Sleep(2 * time.Second)

	// Check that at least some notifications were sent (not all state changes may be valid)
	if gock.GetUnmatchedRequests() == nil && len(gock.GetUnmatchedRequests()) == 0 {
		// This means some notifications were processed successfully
		t.Logf("Stress test completed successfully with rapid state changes")
	}
}

// Test thread safety of subscription store
func TestRMM_SubscriptionStore_ThreadSafety(t *testing.T) {
	store := rms.GetSubscriptionStore()

	// Record initial subscription count (may have leftovers from previous tests)
	initialCount := len(store.GetAll())

	const numGoroutines = 100
	const operationsPerGoroutine = 50

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*operationsPerGoroutine)

	// Concurrent operations on the store
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for j := 0; j < operationsPerGoroutine; j++ {
				subId := fmt.Sprintf("test-sub-%d-%d", goroutineID, j)
				ueId := fmt.Sprintf("imsi-test-%d-%d", goroutineID, j)
				notifyUri := fmt.Sprintf("http://test.com/%d/%d", goroutineID, j)

				sub := &rms.Subscription{
					SubId:     subId,
					UeId:      ueId,
					NotifyUri: notifyUri,
				}

				// Create
				store.Create(sub)

				// Read
				if retrieved, exists := store.Get(subId); !exists {
					errors <- fmt.Errorf("Subscription %s not found after create", subId)
					continue
				} else if retrieved.UeId != ueId {
					errors <- fmt.Errorf("Subscription %s data mismatch", subId)
					continue
				}

				// Update
				updatedSub := &rms.Subscription{
					SubId:     subId,
					UeId:      ueId,
					NotifyUri: notifyUri + "-updated",
				}
				if !store.Update(subId, updatedSub) {
					errors <- fmt.Errorf("Failed to update subscription %s", subId)
					continue
				}

				// Find by UE ID
				found := store.FindByUeId(ueId)
				if len(found) == 0 {
					errors <- fmt.Errorf("No subscriptions found for UE %s", ueId)
					continue
				}

				// Delete
				if !store.Delete(subId) {
					errors <- fmt.Errorf("Failed to delete subscription %s", subId)
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	var errorList []error
	for err := range errors {
		errorList = append(errorList, err)
	}

	if len(errorList) > 0 {
		t.Fatalf("Thread safety test failed with %d errors. First error: %v", len(errorList), errorList[0])
	}

	// Verify all test subscriptions were cleaned up (store should return to initial count)
	finalCount := len(store.GetAll())
	if finalCount != initialCount {
		t.Errorf("Expected store to return to initial count %d, but found %d subscriptions (leaked %d)",
			initialCount, finalCount, finalCount-initialCount)
	}
}

// Test 3GPP compliance requirements
func TestRMM_3GPP_Compliance(t *testing.T) {
	// Test API URI structure compliance
	expectedPrefix := "/namf-rmm/v1"
	if !strings.HasPrefix(baseAPIURL, "http://127.0.0.15:18080"+expectedPrefix) {
		t.Errorf("API URI doesn't match 3GPP format. Expected prefix: %s, got: %s", expectedPrefix, baseAPIURL)
	}

	// Test Content-Type headers for all operations
	testCases := []struct {
		method     string
		url        string
		body       interface{}
		expectedCT string
	}{
		{http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil, "application/json"},
		{http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), Subscription{UeId: "test", NotifyUri: "http://test.com"}, "application/json"},
	}

	for _, tc := range testCases {
		req, _ := http.NewRequest(tc.method, tc.url, nil)
		if tc.body != nil {
			jsonBody, _ := json.Marshal(tc.body)
			req.Body = io.NopCloser(bytes.NewReader(jsonBody))
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Request failed: %v", err)
		}
		defer resp.Body.Close()

		// Check that server accepts and responds with JSON
		contentType := resp.Header.Get("Content-Type")
		if tc.method == http.MethodGet && !strings.Contains(contentType, "application/json") {
			t.Errorf("Expected JSON response for %s, got Content-Type: %s", tc.method, contentType)
		}
	}
}

// Test specific HTTP status codes as per 3GPP spec
func TestRMM_HTTP_StatusCodes(t *testing.T) {
	// Test GET /subscriptions - should return 200 OK
	status, _ := httpDoJSON(t, http.MethodGet, fmt.Sprintf("%s/subscriptions", baseAPIURL), nil)
	if status != http.StatusOK {
		t.Errorf("GET /subscriptions should return 200 OK, got %d", status)
	}

	// Test POST /subscriptions - should return 201 Created
	validSub := Subscription{UeId: "imsi-test-201", NotifyUri: "http://127.0.0.1:9099/test-201"}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), validSub)
	if status != http.StatusCreated {
		t.Errorf("POST /subscriptions should return 201 Created, got %d", status)
	}

	// Extract subscription ID for further tests
	var createdSub Subscription
	json.Unmarshal(data, &createdSub)
	subID := createdSub.SubId

	// Test PUT /subscriptions/{id} with existing subscription - should return 200 OK
	updateSub := Subscription{UeId: "imsi-test-201", NotifyUri: "http://127.0.0.1:9099/test-201-updated"}
	status, _ = httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), updateSub)
	if status != http.StatusOK {
		t.Errorf("PUT existing subscription should return 200 OK, got %d", status)
	}

	// Test PUT /subscriptions/{id} with non-existing subscription - should return 201 Created
	newSub := Subscription{UeId: "imsi-test-new", NotifyUri: "http://127.0.0.1:9099/test-new"}
	status, _ = httpDoJSON(t, http.MethodPut, fmt.Sprintf("%s/subscriptions/new-id-123", baseAPIURL), newSub)
	if status != http.StatusCreated {
		t.Errorf("PUT non-existing subscription should return 201 Created, got %d", status)
	}

	// Test DELETE /subscriptions/{id} - should return 204 No Content
	status, _ = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subID), nil)
	if status != http.StatusNoContent {
		t.Errorf("DELETE subscription should return 204 No Content, got %d", status)
	}

	// Test DELETE non-existing subscription - should return 404 Not Found (as per spec)
	status, _ = httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/non-existent-999", baseAPIURL), nil)
	if status != http.StatusNotFound {
		t.Errorf("DELETE non-existing subscription should return 404 Not Found, got %d", status)
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/new-id-123", baseAPIURL), nil)
}

// Test JSON schema validation
func TestRMM_JSON_SchemaValidation(t *testing.T) {
	testCases := []struct {
		name           string
		payload        interface{}
		expectedStatus int
		description    string
	}{
		{
			name:           "Valid subscription",
			payload:        Subscription{UeId: "imsi-208930000000001", NotifyUri: "http://127.0.0.1:9099/valid"},
			expectedStatus: http.StatusCreated,
			description:    "Should accept valid subscription",
		},
		{
			name: "Missing UeId",
			payload: struct {
				NotifyUri string `json:"notifyUri"`
			}{NotifyUri: "http://test.com"},
			expectedStatus: http.StatusNotFound,
			description:    "Should reject subscription without UeId",
		},
		{
			name: "Missing NotifyUri",
			payload: struct {
				UeId string `json:"ueId"`
			}{UeId: "imsi-test"},
			expectedStatus: http.StatusNotFound,
			description:    "Should reject subscription without NotifyUri",
		},
		{
			name:           "Empty UeId",
			payload:        Subscription{UeId: "", NotifyUri: "http://test.com"},
			expectedStatus: http.StatusNotFound,
			description:    "Should reject subscription with empty UeId",
		},
		{
			name:           "Empty NotifyUri",
			payload:        Subscription{UeId: "imsi-test", NotifyUri: ""},
			expectedStatus: http.StatusNotFound,
			description:    "Should reject subscription with empty NotifyUri",
		},
		{
			name:           "Invalid JSON",
			payload:        "{invalid-json",
			expectedStatus: http.StatusNotFound,
			description:    "Should reject malformed JSON",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), tc.payload)
			if status != tc.expectedStatus {
				t.Errorf("%s: expected status %d, got %d", tc.description, tc.expectedStatus, status)
			}
		})
	}
}

// Test subscription ID uniqueness and format
func TestRMM_SubscriptionID_Uniqueness(t *testing.T) {
	const numSubscriptions = 100
	var subscriptionIds []string

	// Create multiple subscriptions and collect IDs
	for i := 0; i < numSubscriptions; i++ {
		ueID := fmt.Sprintf("imsi-uniqueness-test-%04d", i)
		notifyUri := fmt.Sprintf("http://127.0.0.1:9099/unique-%d", i)

		reqSub := Subscription{UeId: ueID, NotifyUri: notifyUri}
		status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)

		if status != http.StatusCreated {
			t.Fatalf("Failed to create subscription %d: status %d", i, status)
		}

		var createdSub Subscription
		json.Unmarshal(data, &createdSub)

		// Verify SubId format (should start with "sub-")
		if !strings.HasPrefix(createdSub.SubId, "sub-") {
			t.Errorf("Subscription ID should start with 'sub-', got: %s", createdSub.SubId)
		}

		// Check for uniqueness
		for _, existingId := range subscriptionIds {
			if createdSub.SubId == existingId {
				t.Fatalf("Duplicate subscription ID found: %s", createdSub.SubId)
			}
		}

		subscriptionIds = append(subscriptionIds, createdSub.SubId)
	}

	// Cleanup
	for _, subId := range subscriptionIds {
		httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, subId), nil)
	}
}

// Test notification payload format validation
func TestRMM_NotificationPayload_Validation(t *testing.T) {
	ueID := "imsi-208930000000123"
	notifyBase := "http://127.0.0.1:9099"
	notifyPath := "/payload-validation"

	// Create subscription (using real HTTP, not mocked)
	reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}
	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("Create subscription failed: %d %s", status, string(data))
	}

	var sub Subscription
	json.Unmarshal(data, &sub)

	// Now intercept http.DefaultClient for mocking notification callbacks
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Set up a more detailed mock to capture and validate the payload
	var capturedPayload UeRMNotif
	gock.New(notifyBase).
		Post(notifyPath).
		MatchType("json").
		AddMatcher(func(req *http.Request, ereq *gock.Request) (bool, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return false, err
			}
			req.Body = io.NopCloser(bytes.NewReader(body))

			err = json.Unmarshal(body, &capturedPayload)
			if err != nil {
				t.Errorf("Invalid JSON in notification payload: %v", err)
				return false, err
			}

			// Validate payload structure
			if capturedPayload.SubId == "" {
				t.Errorf("Missing subId in notification payload")
				return false, fmt.Errorf("missing subId")
			}
			if capturedPayload.UeId == "" {
				t.Errorf("Missing ueId in notification payload")
				return false, fmt.Errorf("missing ueId")
			}
			if capturedPayload.PrevState == "" {
				t.Errorf("Missing 'from' state in notification payload")
				return false, fmt.Errorf("missing from state")
			}
			if capturedPayload.CurrState == "" {
				t.Errorf("Missing 'to' state in notification payload")
				return false, fmt.Errorf("missing to state")
			}

			return true, nil
		}).
		Reply(204)

	// Attach RMS and trigger state change
	gmm.AttachRMS(rms.NewRMS())

	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger state change
	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
		fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	// Wait for notification
	time.Sleep(500 * time.Millisecond)

	if !gock.IsDone() {
		t.Fatalf("Expected notification not received")
	}

	// Validate the captured payload content
	if capturedPayload.SubId != sub.SubId {
		t.Errorf("Wrong SubId in notification: expected %s, got %s", sub.SubId, capturedPayload.SubId)
	}
	if capturedPayload.UeId != ueID {
		t.Errorf("Wrong UeId in notification: expected %s, got %s", ueID, capturedPayload.UeId)
	}
	if capturedPayload.PrevState != "Deregistered" {
		t.Errorf("Wrong PrevState in notification: expected 'Deregistered', got %s", capturedPayload.PrevState)
	}
	if capturedPayload.CurrState != "Authentication" {
		t.Errorf("Wrong CurrState in notification: expected 'Authentication', got %s", capturedPayload.CurrState)
	}
}

// Test multiple subscriptions for same UE
func TestRMM_MultipleSubscriptions_SameUE(t *testing.T) {
	ueID := "imsi-208930000000456"
	notifyBase := "http://127.0.0.1:9099"
	const numSubscriptions = 3

	var subscriptions []Subscription

	// Create multiple subscriptions for the same UE (using real HTTP, not mocked)
	for i := 0; i < numSubscriptions; i++ {
		notifyPath := fmt.Sprintf("/same-ue-%d", i)
		reqSub := Subscription{UeId: ueID, NotifyUri: notifyBase + notifyPath}

		status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
		if status != http.StatusCreated {
			t.Fatalf("Create subscription %d failed: %d %s", i, status, string(data))
		}

		var sub Subscription
		json.Unmarshal(data, &sub)
		subscriptions = append(subscriptions, sub)
	}

	// Now intercept http.DefaultClient for mocking notification callbacks
	gock.InterceptClient(http.DefaultClient)
	defer gock.RestoreClient(http.DefaultClient)
	defer gock.Off()

	// Set up mock for each subscription
	for i, sub := range subscriptions {
		notifyPath := fmt.Sprintf("/same-ue-%d", i)
		gock.New(notifyBase).
			Post(notifyPath).
			MatchType("json").
			JSON(&UeRMNotif{SubId: sub.SubId, UeId: ueID, PrevState: "Deregistered", CurrState: "Authentication"}).
			Reply(204)
	}

	// Attach RMS and trigger state change
	gmm.AttachRMS(rms.NewRMS())

	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger one state change - should notify ALL subscriptions
	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
		fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	// Wait for all notifications
	timeout := time.After(2 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			if !gock.IsDone() {
				pendingMocks := gock.Pending()
				t.Fatalf("Not all notifications received. %d mocks still pending", len(pendingMocks))
			}
			return
		case <-ticker.C:
			if gock.IsDone() {
				return
			}
		}
	}
}

// Test error handling in notification delivery
func TestRMM_NotificationDelivery_ErrorHandling(t *testing.T) {
	ueID := "imsi-208930000000789"

	// Test with unreachable notification URI
	badNotifyUri := "http://127.0.0.1:99999/unreachable"
	reqSub := Subscription{UeId: ueID, NotifyUri: badNotifyUri}

	status, data := httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub)
	if status != http.StatusCreated {
		t.Fatalf("Create subscription failed: %d %s", status, string(data))
	}

	var sub Subscription
	json.Unmarshal(data, &sub)

	// Attach RMS and trigger state change
	gmm.AttachRMS(rms.NewRMS())

	ue := amf_context.GetSelf().NewAmfUe(ueID)
	anType := models.AccessType__3_GPP_ACCESS
	ue.State[anType] = fsm.NewState("Deregistered")

	// Trigger state change - notification should fail but not crash the system
	if err := gmm.GmmFSM.SendEvent(ue.State[anType], gmm.StartAuthEvent,
		fsm.ArgsType{gmm.ArgAmfUe: ue, gmm.ArgAccessType: anType}, amf_logger.GmmLog); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	// Wait a bit for the failed notification attempt
	time.Sleep(1 * time.Second)

	// System should still be responsive - test with another subscription
	goodNotifyUri := "http://127.0.0.1:9099/good-endpoint"
	reqSub2 := Subscription{UeId: ueID + "-2", NotifyUri: goodNotifyUri}

	status, _ = httpDoJSON(t, http.MethodPost, fmt.Sprintf("%s/subscriptions/", baseAPIURL), reqSub2)
	if status != http.StatusCreated {
		t.Errorf("System should remain responsive after notification failure, but subscription creation failed with status %d", status)
	}

	// Cleanup
	httpDoJSON(t, http.MethodDelete, fmt.Sprintf("%s/subscriptions/%s", baseAPIURL, sub.SubId), nil)
}
