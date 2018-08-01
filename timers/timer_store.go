package timers

/* This module returns only common.ErrRetryTimeout error */

import (
	"encoding/asn1"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/couchbase/eventing/logging"
	"github.com/couchbase/gocb"
	"golang.org/x/crypto/ripemd160"
)

// Constants
const (
	Resolution  = int64(7) // seconds
	init_seq    = int64(128)
	dict        = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789*&"
	encode_base = 10
)

// Globals
var (
	stores *storeMap = newStores()
)

type storeMap struct {
	lock    sync.RWMutex
	entries map[string]*TimerStore
}

type AlarmRecord struct {
	AlarmDue   int64  `json:"due"`
	ContextRef string `json:"context_ref"`
}

type ContextRecord struct {
	Context  interface{} `json:"context"`
	AlarmRef string      `json:"alarm_ref"`
}

type TimerEntry struct {
	AlarmRecord
	ContextRecord

	alarmSeq int64
	ctxCas   gocb.Cas
	topCas   gocb.Cas
}

type rowIter struct {
	start   int64
	stop    int64
	current int64
}

type colIter struct {
	stop    int64
	current int64
	topCas  gocb.Cas
}

type Span struct {
	Start int64 `json:"start"`
	Stop  int64 `json:"stop"`
}

type storeSpan struct {
	Span
	empty   bool
	spanCas gocb.Cas
	lock    sync.Mutex
}

type TimerStore struct {
	connstr string
	bucket  string
	prefix  string
	handler string
	partn   int
	log     string
	span    storeSpan
	sync    *time.Ticker
}

type TimerIter struct {
	store *TimerStore
	row   rowIter
	col   *colIter
	entry *TimerEntry
}

func Create(prefix, handler string, partn int, connstr string, bucket string) error {
	_, ok := Fetch(handler, partn)
	if ok {
		logging.Warnf("Asked to create store %v:%v which exists. Reusing", handler, partn)
		return nil
	}
	store, err := newTimerStore(prefix, handler, partn, connstr, bucket)
	if err != nil {
		return err
	}
	stores.lock.Lock()
	defer stores.lock.Unlock()
	stores.entries[mapLocator(handler, partn)] = store
	return nil
}

func Fetch(handler string, partn int) (store *TimerStore, found bool) {
	stores.lock.RLock()
	defer stores.lock.RUnlock()
	store, found = stores.entries[mapLocator(handler, partn)]
	if !found {
		logging.Infof("Store not defined: " + mapLocator(handler, partn))
		return nil, false
	}
	return
}

func (r *TimerStore) Free() {
	stores.lock.Lock()
	defer stores.lock.Unlock()
	r.sync.Stop()
	delete(stores.entries, mapLocator(r.handler, r.partn))
}

func (r *TimerStore) Set(due int64, ref string, context interface{}) error {
	now := time.Now().Unix()
	if due-now <= Resolution {
		logging.Warnf("%v Moving too close/past timer to next period: %v context %ru", r.log, formatTime(due), context)
		due = now + Resolution
	}
	due = roundUp(due)

	kv := Pool(r.connstr)
	pos := r.kvLocatorRoot(due)
	seq, _, err := kv.MustCounter(r.bucket, pos, 1, init_seq, 0)
	if err != nil {
		return err
	}

	akey := r.kvLocatorAlarm(due, seq)
	ckey := r.kvLocatorContext(ref)

	arecord := AlarmRecord{AlarmDue: due, ContextRef: ckey}
	_, err = kv.MustUpsert(r.bucket, akey, arecord, 0)
	if err != nil {
		return err
	}

	crecord := ContextRecord{Context: context, AlarmRef: akey}
	_, err = kv.MustUpsert(r.bucket, ckey, crecord, 0)
	if err != nil {
		return err
	}

	logging.Tracef("%v Creating timer at %v seq %v with ref %ru and context %ru", r.log, seq, formatTime(due), ref, context)
	r.expandSpan(due)
	return nil
}

func (r *TimerStore) Delete(entry *TimerEntry) error {
	logging.Tracef("%v Deleting timer %+v", r.log, entry)
	kv := Pool(r.connstr)

	_, absent, _, err := kv.MustRemove(r.bucket, entry.AlarmRef, 0)
	if err != nil {
		return err
	}
	if absent {
		logging.Warnf("%v Timer %v seq %v is missing alarm in del: %ru", r.log, entry.AlarmDue, entry.alarmSeq, *entry)
	}

	_, _, mismatch, err := kv.MustRemove(r.bucket, entry.ContextRef, entry.ctxCas)
	if err != nil {
		return err
	}
	if mismatch {
		logging.Warnf("%v Timer %v seq %v was either cancelled or overriden after it fired: %ru", r.log, entry.AlarmDue, entry.alarmSeq, *entry)
		return nil
	}

	if entry.topCas == 0 {
		return nil
	}

	pos := r.kvLocatorRoot(entry.AlarmDue)
	logging.Debugf("%v Removing last entry, so removing counter %+v", r.log, pos)

	_, absent, mismatch, err = kv.MustRemove(r.bucket, pos, entry.topCas)
	if err != nil {
		return err
	}
	if absent || mismatch {
		logging.Tracef("%v Concurrency on %v absent:%v mismatch:%v", r.log, pos, absent, mismatch)
	}

	r.shrinkSpan(entry.AlarmDue)
	return nil
}

func (r *TimerStore) Cancel(ref string) error {
	logging.Tracef("%v Cancelling timer ref %ru", r.log, ref)

	kv := Pool(r.connstr)
	cref := r.kvLocatorContext(ref)

	crecord := ContextRecord{}
	_, absent, err := kv.MustGet(r.bucket, cref, &crecord)
	if err != nil {
		return nil
	}
	if absent {
		logging.Tracef("%v Timer asked to cancel %ru cref %v does not exist", r.log, ref, cref)
		return nil
	}

	_, absent, _, err = kv.MustRemove(r.bucket, crecord.AlarmRef, 0)
	if err != nil {
		return nil
	}
	if absent {
		logging.Tracef("%v Timer asked to cancel %ru alarmref %v does not exist", r.log, ref, crecord.AlarmRef)
		return nil
	}

	_, absent, _, err = kv.MustRemove(r.bucket, cref, 0)
	if err != nil {
		return nil
	}
	if absent {
		logging.Tracef("%v Timer asked to cancel %ru cref %v does not exist", r.log, ref, cref)
	}

	return nil
}

func (r *TimerStore) ScanDue() (*TimerIter, error) {
	span := r.readSpan()
	now := roundDown(time.Now().Unix())

	if span.Start == span.Stop && now-span.Stop > 3*Resolution {
		logging.Tracef("%v No more timers. Not creating iterator: %+v", r.log, span)
		return nil, nil
	}

	stop := now
	if stop > span.Stop {
		stop = span.Stop
	}

	iter := TimerIter{
		store: r,
		entry: nil,
		row: rowIter{
			start:   span.Start,
			current: span.Start,
			stop:    stop,
		},
		col: nil,
	}

	logging.Tracef("%v Created iterator: %+v", r.log, iter)
	return &iter, nil
}

func (r *TimerIter) ScanNext() (*TimerEntry, error) {
	if r == nil {
		return nil, nil
	}

	for {
		found, err := r.nextColumn()
		if err != nil {
			return nil, err
		}
		if found {
			return r.entry, nil
		}
		found, err = r.nextRow()
		if !found || err != nil {
			return nil, err
		}
	}
}

func (r *TimerIter) nextRow() (bool, error) {
	logging.Tracef("%v Looking for row after %+v", r.store.log, r.row)
	kv := Pool(r.store.connstr)

	r.col = nil
	r.entry = nil

	col := colIter{current: init_seq, topCas: 0}
	for r.row.current < r.row.stop {
		r.row.current += Resolution

		pos := r.store.kvLocatorRoot(r.row.current)
		cas, absent, err := kv.MustGet(r.store.bucket, pos, &col.stop)
		if err != nil {
			return false, err
		}
		if !absent {
			col.topCas = cas
			r.col = &col
			logging.Tracef("%v Found row %+v", r.store.log, r.row)
			return true, nil
		}
	}
	logging.Tracef("%v Found no rows looking until %v", r.store.log, r.row.stop)
	return false, nil
}

func (r *TimerIter) nextColumn() (bool, error) {
	logging.Tracef("%v Looking for column after %+v in row %+v", r.store.log, r.col, r.row)
	r.entry = nil

	if r.col == nil {
		return false, nil
	}

	kv := Pool(r.store.connstr)
	alarm := AlarmRecord{}
	context := ContextRecord{}

	for r.col.current <= r.col.stop {
		current := r.col.current
		r.col.current++

		key := r.store.kvLocatorAlarm(r.row.current, current)

		_, absent, err := kv.MustGet(r.store.bucket, key, &alarm)
		if err != nil {
			return false, err
		}
		if absent {
			logging.Debugf("%v Skipping missing entry in chain at %v", r.store.log, key)
			continue
		}

		cas, absent, err := kv.MustGet(r.store.bucket, alarm.ContextRef, &context)
		if err != nil {
			return false, err
		}
		if absent || context.AlarmRef != key {
			// Alarm canceled if absent, or superceded if AlarmRef != key
			logging.Debugf("%v Alarm canceled or superceded %v by context %ru", r.store.log, alarm, context)
			continue
		}

		r.entry = &TimerEntry{AlarmRecord: alarm, ContextRecord: context, alarmSeq: current, topCas: 0, ctxCas: cas}
		if current == r.col.stop {
			r.entry.topCas = r.col.topCas
		}
		logging.Tracef("%v Scan returning timer %+v", r.store.log, r.entry)
		return true, nil

	}

	logging.Tracef("%v Column scan finished for %+v", r.store.log, r)
	return false, nil
}

func (r *TimerStore) readSpan() Span {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	return r.span.Span
}

func (r *TimerStore) expandSpan(point int64) {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	if r.span.Start > point {
		r.span.Start = point
	}
	if r.span.Stop < point {
		r.span.Stop = point
	}
}

func (r *TimerStore) shrinkSpan(start int64) {
	r.span.lock.Lock()
	defer r.span.lock.Unlock()
	if r.span.Start < start {
		r.span.Start = start
	}
}

func (r *TimerStore) syncSpan() error {
	kv := Pool(r.connstr)
	pos := r.kvLocatorSpan()
	extspan := Span{}

	rcas, absent, err := kv.MustGet(r.bucket, pos, &extspan)
	if err != nil {
		return err
	}

	r.span.lock.Lock()
	defer r.span.lock.Unlock()

	switch {
	// brand new
	case absent && r.span.empty:
		now := time.Now().Unix()
		r.span.Span = Span{Start: roundDown(now), Stop: roundUp(now)}
		r.span.empty = false
		r.span.spanCas = 0
		logging.Infof("%v Span initialized for the first time %+v", r.log, r.span)

	// never persisted, but we have data
	case absent && !r.span.empty:
		wcas, mismatch, err := kv.MustInsert(r.bucket, pos, r.span.Span, 0)
		if err != nil || mismatch {
			return err
		}
		r.span.spanCas = wcas
		return err

	// we have no data, but some persisted
	case !absent && r.span.empty:
		r.span.empty = false
		r.span.Span = extspan
		r.span.spanCas = rcas
		logging.Tracef("%v Span missing, and was initialized to %+v", r.log, r.span)

	// we have data and see external changes
	case r.span.spanCas != rcas:
		logging.Warnf("%v Span was changed externally, merging %+v and %+v", r.log, extspan, r.span)
		if r.span.Start > extspan.Start {
			r.span.Start = extspan.Start
		}
		if r.span.Stop < extspan.Stop {
			r.span.Stop = extspan.Stop
		}

	// nothing has moved
	case r.span.spanCas == rcas && r.span.Span == extspan:
		logging.Tracef("%v Span no changes %+v", r.log, r.span)
		return nil

	// only internal changes
	default:
		logging.Tracef("%v Span no conflict %+v", r.log, r.span)
	}

	wcas, absent, mismatch, err := kv.MustReplace(r.bucket, pos, r.span.Span, rcas, 0)
	if err != nil {
		return err
	}
	if absent || mismatch {
		logging.Warnf("%v Span was changed again externally, not commiting merged span %+v", r.log, r.span)
		return nil
	}

	r.span.spanCas = wcas
	logging.Tracef("%v Span was merged and saved successfully: %+v", r.log, r.span)
	return nil
}

func (r *TimerStore) syncRoutine() {
	for _ = range r.sync.C {
		err := r.syncSpan()
		if err != nil {
			return
		}
	}
}

func newTimerStore(prefix, handler string, partn int, connstr string, bucket string) (*TimerStore, error) {
	timerstore := TimerStore{
		connstr: connstr,
		bucket:  bucket,
		prefix:  prefix,
		handler: handler,
		partn:   partn,
		log:     fmt.Sprintf("timerstore:%v:%v:%v", prefix, handler, partn),
		sync:    time.NewTicker(time.Duration(Resolution) * time.Second),
		span:    storeSpan{empty: true},
	}

	err := timerstore.syncSpan()
	if err != nil {
		return nil, err
	}

	go timerstore.syncRoutine()

	logging.Infof("%v Initialized timerdata store", timerstore.log)
	return &timerstore, nil
}

func (r *TimerStore) kvLocatorRoot(due int64) string {
	return fmt.Sprintf("%v:timerstore:%v:%v:root:%v", r.prefix, r.handler, r.partn, formatTime(due))
}

func (r *TimerStore) kvLocatorAlarm(due int64, seq int64) string {
	return fmt.Sprintf("%v:timerstore:%v:%v:alarm:%v:%v", r.prefix, r.handler, r.partn, formatTime(due), seq)
}

func (r *TimerStore) kvLocatorContext(ref string) string {
	Assert(64 == len(dict))
	ripe := ripemd160.New()
	ripe.Write([]byte(ref))
	bits := asn1.BitString{Bytes: ripe.Sum(nil), BitLength: 160}
	hash := ""
	for i := 0; i < 160; i += 5 {
		pos := 0
		for j := 0; j < 5; j++ {
			pos = pos<<1 | bits.At(i+j)
		}
		Assert(pos >= 0 && pos < len(dict))
		hash += string(dict[pos])

	}
	return fmt.Sprintf("%v:timerstore:%v:%v:context:%v", r.prefix, r.handler, r.partn, hash)
}

func (r *TimerStore) kvLocatorSpan() string {
	return fmt.Sprintf("%v:timerstore:%v:%v:span", r.prefix, r.handler, r.partn)
}

func mapLocator(handler string, partn int) string {
	return handler + ":" + strconv.FormatInt(int64(partn), encode_base)
}

func (r *ContextRecord) String() string {
	format := logging.RedactFormat("ContextRecord[AlarmRef=%v, Context=%ru]")
	return fmt.Sprintf(format, r.AlarmRef, r.Context)
}

func formatTime(tm int64) string {
	return strconv.FormatInt(tm, encode_base)
}

func newStores() *storeMap {
	return &storeMap{
		entries: make(map[string]*TimerStore),
		lock:    sync.RWMutex{},
	}
}

func roundUp(val int64) int64 {
	q := val / Resolution
	r := val % Resolution
	if r > 0 {
		q++
	}
	return q * Resolution
}

func roundDown(val int64) int64 {
	q := val / Resolution
	return q * Resolution
}