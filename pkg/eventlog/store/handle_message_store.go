// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package store

import (
	"strings"
	"sync"
	"time"

	cdb "github.com/goodrain/rainbond/pkg/db"
	"github.com/goodrain/rainbond/pkg/db/model"
	"github.com/goodrain/rainbond/pkg/eventlog/conf"
	"github.com/goodrain/rainbond/pkg/eventlog/db"
	"github.com/goodrain/rainbond/pkg/eventlog/exit/webhook"
	"github.com/goodrain/rainbond/pkg/eventlog/util"
	"golang.org/x/net/context"

	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/prometheus/client_golang/prometheus"
)

type handleMessageStore struct {
	barrels                map[string]*EventBarrel
	lock                   sync.RWMutex
	garbageLock            sync.Mutex
	conf                   conf.EventStoreConf
	log                    *logrus.Entry
	garbageMessage         []*db.EventLogMessage
	garbageGC              chan int
	ctx                    context.Context
	barrelEvent            chan []string
	dbPlugin               db.Manager
	cancel                 func()
	handleEventCoreSize    int
	stopGarbage            chan struct{}
	pool                   *sync.Pool
	manager                *storeManager
	size                   int64
	allLogCount, allBarrel float64
}

func (h *handleMessageStore) Run() {
	go h.handleBarrelEvent()
	go h.Gc()
}
func (h *handleMessageStore) Scrape(ch chan<- prometheus.Metric, namespace, exporter, from string) error {
	chanDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "event_store_cache_barrel_count"),
		"cache event barrel count.",
		[]string{"from"}, nil,
	)
	ch <- prometheus.MustNewConstMetric(chanDesc, prometheus.GaugeValue, float64(len(h.barrels)), from)
	logDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "event_store_log_count"),
		"the handle event log count size.",
		[]string{"from"}, nil,
	)
	ch <- prometheus.MustNewConstMetric(logDesc, prometheus.GaugeValue, h.allLogCount, from)
	barrelDesc := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, exporter, "event_store_barrel_count"),
		"all event barrel count.",
		[]string{"from"}, nil,
	)
	ch <- prometheus.MustNewConstMetric(barrelDesc, prometheus.GaugeValue, h.allBarrel, from)
	return nil
}

func (h *handleMessageStore) GetMonitorData() *db.MonitorData {
	da := &db.MonitorData{
		LogSizePeerM: h.size,
		ServiceSize:  len(h.barrels),
	}
	return da
}

func (h *handleMessageStore) SubChan(eventID, subID string) chan *db.EventLogMessage {
	return nil
}
func (h *handleMessageStore) RealseSubChan(eventID, subID string) {}

//GC 操作进行时 消息接收会停止
//TODD 怎么加快gc?
//使用对象池
func (h *handleMessageStore) Gc() {
	h.log.Debug("Handle message store gc core start.")
	tiker := time.NewTicker(time.Second * 30)
	for {
		select {
		case <-tiker.C:
			h.size = 0
			h.gcRun()
		case <-h.ctx.Done():
			tiker.Stop()
			h.log.Debug("Handle message store gc core stop.")
			return
		}
	}
}
func (h *handleMessageStore) gcRun() {
	//h.log.Debugf("runGC %d", time.Now().UnixNano())
	h.lock.Lock()
	defer h.lock.Unlock()
	t := time.Now()
	if len(h.barrels) == 0 {
		return
	}
	var gcEvent []string
	for k, v := range h.barrels {
		if v.updateTime.Add(time.Second * 30).Before(time.Now()) { // barrel 超时未收到消息
			h.saveBeforeGc(v)
			gcEvent = append(gcEvent, k)
		}
	}
	if gcEvent != nil && len(gcEvent) > 0 {
		for _, id := range gcEvent {
			barrel := h.barrels[id]
			barrel.empty()
			h.pool.Put(barrel) //放回对象池
			delete(h.barrels, id)
		}
	}
	useTime := time.Now().UnixNano() - t.UnixNano()
	h.log.Debugf("Handle message store complete gc in %d ns", useTime)
}

func (h *handleMessageStore) stop() {
	h.cancel()
	h.lock.Lock()
	defer h.lock.Unlock()
	h.log.Debug("start persistence message before handle Message Store stop.")
	for _, v := range h.barrels {
		h.saveBeforeGc(v)
	}
	close(h.stopGarbage)
	h.log.Debug("handle Message Store stop.")
}

// gc删除前持久化数据
func (h *handleMessageStore) saveBeforeGc(v *EventBarrel) {
	v.persistencelock.Lock()
	v.gcPersistence()
	if len(v.persistenceBarrel) > 0 {
		if err := h.dbPlugin.SaveMessage(v.persistenceBarrel); err != nil {
			h.log.Error("persistence barrel message error.", err.Error())
			h.InsertGarbageMessage(v.persistenceBarrel...)
		}
	}
	v.persistenceBarrel = nil
	v.persistencelock.Unlock()
	h.log.Debugf("Handle message store complete gc barrel(%s)", v.eventID)
}
func (h *handleMessageStore) insertMessage(message *db.EventLogMessage) bool {
	h.lock.RLock()
	defer h.lock.RUnlock()
	if barrel, ok := h.barrels[message.EventID]; ok {
		err := barrel.insert(message)
		if err != nil {
			h.log.Warn("insert message to barrel error.", err.Error())
			h.InsertGarbageMessage(message)
		}
		return true
	}
	return false
}
func (h *handleMessageStore) InsertMessage(message *db.EventLogMessage) {
	if message == nil || message.EventID == "" {
		return
	}
	h.size++
	h.allLogCount++
	eventID := message.EventID
	if h.insertMessage(message) {
		return
	}
	h.lock.Lock()
	defer h.lock.Unlock()
	barrel := h.pool.Get().(*EventBarrel)
	barrel.eventID = eventID
	err := barrel.insert(message)
	if err != nil {
		h.log.Warn("insert message to barrel error.", err.Error())
		h.InsertGarbageMessage(message)
	}
	h.barrels[eventID] = barrel
	h.allBarrel++
}

func (h *handleMessageStore) InsertGarbageMessage(message ...*db.EventLogMessage) {
	h.garbageLock.Lock()
	defer h.garbageLock.Unlock()
	h.garbageMessage = append(h.garbageMessage, message...)
}

func (h *handleMessageStore) handleGarbageMessage() {
	tike := time.Tick(10 * time.Second)
	switch h.conf.GarbageMessageSaveType {
	default: //file
		for {
			select {
			case <-tike:
				if len(h.garbageMessage) > 0 {
					h.saveGarbageMessage()
				}
			case <-h.garbageGC:
				h.saveGarbageMessage()
			case <-h.stopGarbage:
				h.saveGarbageMessage()
				h.log.Debug("handle message store garbage message handle-core stop.")
				return
			}
		}
	}
}

func (h *handleMessageStore) saveGarbageMessage() {
	h.garbageLock.Lock()
	defer h.garbageLock.Unlock()
	var content string
	for _, m := range h.garbageMessage {
		if m != nil && m.Content != nil {
			content += fmt.Sprintf("(%s-%s) %s: %s\n", m.Step, m.Level, m.Time, m.Message)
		}
	}
	err := util.AppendToFile(h.conf.GarbageMessageFile, content)
	if err != nil {
		//h.log.Error("Save garbage message to file error.context :\n " + content)
		h.log.Error("Save garbage message to file error.context", err.Error())
	} else {
		h.log.Info("Save the garbage message to file.")
	}
	h.garbageMessage = h.garbageMessage[:0]
}

func (h *handleMessageStore) persistence(eventID string) {
	h.lock.RLock()
	defer h.lock.RUnlock()
	if ba, ok := h.barrels[eventID]; ok {
		ba.persistencelock.Lock()
		if ba.needPersistence {
			if err := h.dbPlugin.SaveMessage(ba.persistenceBarrel); err != nil {
				h.log.Error("persistence barrel message error.", err.Error())
				h.InsertGarbageMessage(ba.persistenceBarrel...)
			}
			h.log.Debugf("persistence barrel(%s) %d message  to db.", eventID, len(ba.persistenceBarrel))
			ba.persistenceBarrel = ba.persistenceBarrel[:0]
			ba.needPersistence = false
		}
		ba.persistencelock.Unlock()
	}
}

//TODD
func (h *handleMessageStore) handleBarrelEvent() {
	for {
		select {
		case event := <-h.barrelEvent:
			if len(event) < 1 {
				continue
			}

			h.log.Debug("Handle message store do event.", event)
			if event[0] == "persistence" { //持久化命令
				if len(event) == 2 {
					h.persistence(event[1])
				}
			}
			if event[0] == "callback" { //回调
				if len(event) == 4 {
					eventID := event[1]
					status := event[2]
					message := event[3]
					webhook.GetManager().RunWebhookWithParameter(webhook.UpDateEventStatus, nil,
						map[string]interface{}{"event_id": eventID, "status": status, "message": message})


					event := model.ServiceEvent{}
					event.EventID = eventID
					event.Status = status
					event.Message = message
					logrus.Infof("updating event %s's status: %s",eventID,status)
					cdb.GetManager().ServiceEventDao().UpdateModel(&event)

					//todo  get version_info by event_id ,update final_status,optional delete
				}
			}
			if event[0] == "code-version" { //代码版本
				if len(event) == 3 {
					eventID := event[1]
					codeVersion := strings.TrimSpace(event[2])
					webhook.GetManager().RunWebhookWithParameter(webhook.UpdateEventCodeVersion, nil,
						map[string]interface{}{"event_id": eventID, "code_version": codeVersion})

					event := model.ServiceEvent{}
					event.EventID = eventID
					event.CodeVersion = codeVersion
					cdb.GetManager().ServiceEventDao().UpdateModel(&event)
					version,_:=cdb.GetManager().VersionInfoDao().GetVersionByEventID(eventID)
					//infos:=strings.Split(codeVersion,":")
					version.CodeVersion=codeVersion
					//for k,v:=range infos{
					//	i:=strings.Split(v," ")
					//	if k==0 {
					//
					//	}
					//	if k==1 {
					//		version.CodeVersion=i[0]
					//
					//	}
					//	if k==2 {
					//		version.Author=i[0]
					//
					//	}
					//	if k == 3 {
					//		version.CommitMsg=v
					//	}
					//}
					cdb.GetManager().VersionInfoDao().UpdateModel(version)
					h.log.Infof("run web hook update code version .event_id %s code_version %s", eventID, codeVersion)
				}
			}
		case <-h.ctx.Done():
			return
		}
	}
}
