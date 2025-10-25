package rms

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/free5gc/amf/internal/logger"
	"github.com/free5gc/util/fsm"
)

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

type SubscriptionStore struct {
	subscriptions map[string]*Subscription
	mutex         sync.RWMutex
}

func NewSubscriptionStore() *SubscriptionStore {
	return &SubscriptionStore{
		subscriptions: make(map[string]*Subscription),
	}
}

func (s *SubscriptionStore) Create(sub *Subscription) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.subscriptions[sub.SubId] = sub
}

func (s *SubscriptionStore) Get(subId string) (*Subscription, bool) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	sub, exists := s.subscriptions[subId]
	return sub, exists
}

func (s *SubscriptionStore) GetAll() []*Subscription {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	subs := make([]*Subscription, 0, len(s.subscriptions))
	for _, sub := range s.subscriptions {
		subs = append(subs, sub)
	}
	return subs
}

func (s *SubscriptionStore) Update(subId string, sub *Subscription) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, exists := s.subscriptions[subId]; exists {
		sub.SubId = subId
		s.subscriptions[subId] = sub
		return true
	}
	return false
}

func (s *SubscriptionStore) Delete(subId string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, exists := s.subscriptions[subId]; exists {
		delete(s.subscriptions, subId)
		return true
	}
	return false
}

func (s *SubscriptionStore) FindByUeId(ueId string) []*Subscription {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	var result []*Subscription
	for _, sub := range s.subscriptions {
		if sub.UeId == ueId {
			result = append(result, sub)
		}
	}
	return result
}

type CustomizedRMS struct {
	store *SubscriptionStore
}

var globalStore *SubscriptionStore
var once sync.Once

func NewRMS() fsm.RMS {
	once.Do(func() {
		globalStore = NewSubscriptionStore()
	})
	return &CustomizedRMS{
		store: globalStore,
	}
}

func GetSubscriptionStore() *SubscriptionStore {
	once.Do(func() {
		globalStore = NewSubscriptionStore()
	})
	return globalStore
}

func (rms *CustomizedRMS) HandleEvent(state *fsm.State, event fsm.EventType, args fsm.ArgsType, trans fsm.Transition) {
	ueId := extractUeId(args)
	if ueId == "" {
		return
	}

	prevState := string(trans.From)
	currState := string(trans.To)

	subscriptions := rms.store.FindByUeId(ueId)
	for _, sub := range subscriptions {
		go rms.sendNotification(sub, ueId, prevState, currState)
	}
}

func (rms *CustomizedRMS) sendNotification(sub *Subscription, ueId, prevState, currState string) {
	notification := UeRMNotif{
		SubId:     sub.SubId,
		UeId:      ueId,
		PrevState: prevState,
		CurrState: currState,
	}

	jsonData, err := json.Marshal(notification)
	if err != nil {
		logger.SBILog.Errorf("Failed to marshal notification: %v", err)
		return
	}

	resp, err := http.Post(sub.NotifyUri, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		logger.SBILog.Errorf("Failed to send notification to %s: %v", sub.NotifyUri, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		logger.SBILog.Warnf("Notification endpoint %s returned status %d", sub.NotifyUri, resp.StatusCode)
	}
}

func extractUeId(args fsm.ArgsType) string {
	if args == nil {
		return ""
	}

	for key, value := range args {
		if key == "ArgAmfUe" {
			if ue, ok := value.(interface{ GetSupi() string }); ok {
				return ue.GetSupi()
			}
		}
	}
	return ""
}
