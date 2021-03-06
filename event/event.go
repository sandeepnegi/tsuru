// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var (
	lockUpdateInterval = 30 * time.Second
	lockExpireTimeout  = 5 * time.Minute
	updater            = lockUpdater{
		addCh:    make(chan *Target),
		removeCh: make(chan *Target),
		once:     &sync.Once{},
	}
	throttlingInfo = map[string]ThrottlingSpec{}

	ErrNotCancelable  = errors.New("event is not cancelable")
	ErrEventNotFound  = errors.New("event not found")
	ErrNoTarget       = ErrValidation("event target is mandatory")
	ErrNoKind         = ErrValidation("event kind is mandatory")
	ErrNoOwner        = ErrValidation("event owner is mandatory")
	ErrNoOpts         = ErrValidation("event opts is mandatory")
	ErrNoInternalKind = ErrValidation("event internal kind is mandatory")
	ErrInvalidOwner   = ErrValidation("event owner must not be set on internal events")
	ErrInvalidKind    = ErrValidation("event kind must not be set on internal events")

	OwnerTypeUser     = ownerType("user")
	OwnerTypeApp      = ownerType("app")
	OwnerTypeInternal = ownerType("internal")

	KindTypePermission = kindType("permission")
	KindTypeInternal   = kindType("internal")
)

type ErrThrottled struct {
	Spec   *ThrottlingSpec
	Target Target
}

func (err ErrThrottled) Error() string {
	var extra string
	if err.Spec.KindName != "" {
		extra = fmt.Sprintf(" %s on", err.Spec.KindName)
	}
	return fmt.Sprintf("event throttled, limit for%s %s %q is %d every %v", extra, err.Target.Name, err.Target.Value, err.Spec.Max, err.Spec.Time)
}

type ErrValidation string

func (err ErrValidation) Error() string {
	return string(err)
}

type ErrEventLocked struct{ event *Event }

func (err ErrEventLocked) Error() string {
	return fmt.Sprintf("event locked: %v", err.event)
}

type Target struct{ Name, Value string }

func (id Target) GetBSON() (interface{}, error) {
	return bson.D{{"name", id.Name}, {"value", id.Value}}, nil
}

func (id Target) IsValid() bool {
	return id.Name != "" && id.Value != ""
}

type eventId struct {
	Target Target
	ObjId  bson.ObjectId
}

func (id *eventId) SetBSON(raw bson.Raw) error {
	err := raw.Unmarshal(&id.Target)
	if err != nil {
		return raw.Unmarshal(&id.ObjId)
	}
	return nil
}

func (id eventId) GetBSON() (interface{}, error) {
	if len(id.ObjId) != 0 {
		return id.ObjId, nil
	}
	return id.Target.GetBSON()
}

// This private type allow us to export the main Event struct without allowing
// access to its public fields. (They have to be public for database
// serializing).
type eventData struct {
	ID              eventId `bson:"_id"`
	UniqueID        bson.ObjectId
	StartTime       time.Time
	EndTime         time.Time   `bson:",omitempty"`
	Target          Target      `bson:",omitempty"`
	StartCustomData interface{} `bson:",omitempty"`
	EndCustomData   interface{} `bson:",omitempty"`
	OtherCustomData interface{} `bson:",omitempty"`
	Kind            kind
	Owner           Owner
	Cancelable      bool
	Running         bool
	LockUpdateTime  time.Time
	Error           string
	Log             string `bson:",omitempty"`
	CancelInfo      cancelInfo
	RemoveDate      time.Time `bson:",omitempty"`
}

type cancelInfo struct {
	Owner     string
	StartTime time.Time
	AckTime   time.Time
	Reason    string
	Asked     bool
	Canceled  bool
}

type ownerType string

type kindType string

type Owner struct {
	Type ownerType
	Name string
}

type kind struct {
	Type kindType
	Name string
}

func (o Owner) String() string {
	return fmt.Sprintf("%s %s", o.Type, o.Name)
}

func (k kind) String() string {
	return k.Name
}

type ThrottlingSpec struct {
	TargetName string
	KindName   string
	Max        int
	Time       time.Duration
}

func SetThrottling(spec ThrottlingSpec) {
	key := spec.TargetName
	if spec.KindName != "" {
		key = fmt.Sprintf("%s_%s", spec.TargetName, spec.KindName)
	}
	throttlingInfo[key] = spec
}

func getThrottling(t *Target, k *kind) *ThrottlingSpec {
	key := fmt.Sprintf("%s_%s", t.Name, k.Name)
	if s, ok := throttlingInfo[key]; ok {
		return &s
	}
	if s, ok := throttlingInfo[t.Name]; ok {
		return &s
	}
	return nil
}

type Event struct {
	eventData
	logBuffer safe.Buffer
	logWriter io.Writer
}

type Opts struct {
	Target       Target
	Kind         *permission.PermissionScheme
	InternalKind string
	Owner        auth.Token
	RawOwner     Owner
	Cancelable   bool
	CustomData   interface{}
}

func (e *Event) String() string {
	return fmt.Sprintf("%s(%s) running %q start by %s at %s",
		e.Target.Name,
		e.Target.Value,
		e.Kind,
		e.Owner,
		e.StartTime.Format(time.RFC3339),
	)
}

type Filter struct {
	Target         Target
	KindType       kindType
	KindName       string
	OwnerType      ownerType
	OwnerName      string
	Since          time.Time
	Until          time.Time
	Running        *bool
	IncludeRemoved bool
	Raw            bson.M

	Limit int
	Skip  int
	Sort  string
}

func (f *Filter) toQuery() bson.M {
	query := bson.M{}
	if f.Target.Name != "" {
		query["target.name"] = f.Target.Name
	}
	if f.Target.Value != "" {
		query["target.value"] = f.Target.Value
	}
	if f.KindType != "" {
		query["kind.type"] = f.KindType
	}
	if f.KindName != "" {
		query["kind.name"] = f.KindName
	}
	if f.OwnerType != "" {
		query["owner.type"] = f.OwnerType
	}
	if f.OwnerName != "" {
		query["owner.name"] = f.OwnerName
	}
	var timeParts []bson.M
	if !f.Since.IsZero() {
		timeParts = append(timeParts, bson.M{"starttime": bson.M{"$gte": f.Since}})
	}
	if !f.Until.IsZero() {
		timeParts = append(timeParts, bson.M{"starttime": bson.M{"$lte": f.Until}})
	}
	if len(timeParts) != 0 {
		query["$and"] = timeParts
	}
	if f.Running != nil {
		query["running"] = *f.Running
	}
	if !f.IncludeRemoved {
		query["removedate"] = bson.M{"$exists": false}
	}
	if f.Raw != nil {
		for k, v := range f.Raw {
			query[k] = v
		}
	}
	return query
}

func GetRunning(target Target, kind string) (*Event, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	var evt Event
	err = coll.Find(bson.M{
		"_id":       eventId{Target: target},
		"kind.name": kind,
		"running":   true,
	}).One(&evt.eventData)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, ErrEventNotFound
		}
		return nil, err
	}
	return &evt, nil
}

func GetByID(id bson.ObjectId) (*Event, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	var evt Event
	err = coll.Find(bson.M{
		"uniqueid": id,
	}).One(&evt.eventData)
	if err != nil {
		if err == mgo.ErrNotFound {
			return nil, ErrEventNotFound
		}
		return nil, err
	}
	return &evt, nil
}

func All() ([]Event, error) {
	return List(nil)
}

func List(filter *Filter) ([]Event, error) {
	limit := 100
	skip := 0
	var query bson.M
	sort := "-starttime"
	if filter != nil {
		if filter.Limit != 0 {
			limit = filter.Limit
		}
		if filter.Sort != "" {
			sort = filter.Sort
		}
		if filter.Skip > 0 {
			skip = filter.Skip
		}
		query = filter.toQuery()
	}
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	find := coll.Find(query).Sort(sort)
	if limit > 0 {
		find = find.Limit(limit)
	}
	if skip > 0 {
		find = find.Skip(skip)
	}
	var allData []eventData
	err = find.All(&allData)
	if err != nil {
		return nil, err
	}
	evts := make([]Event, len(allData))
	for i := range evts {
		evts[i].eventData = allData[i]
	}
	return evts, nil
}

func MarkAsRemoved(target Target) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	now := time.Now().UTC()
	_, err = coll.UpdateAll(bson.M{
		"target":     target,
		"removedate": bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{"removedate": now}})
	return err
}

func New(opts *Opts) (*Event, error) {
	if opts == nil {
		return nil, ErrNoOpts
	}
	if opts.Owner == nil && opts.RawOwner.Name == "" && opts.RawOwner.Type == "" {
		return nil, ErrNoOwner
	}
	if opts.Kind == nil {
		return nil, ErrNoKind
	}
	return newEvt(opts)
}

func NewInternal(opts *Opts) (*Event, error) {
	if opts == nil {
		return nil, ErrNoOpts
	}
	if opts.Owner != nil {
		return nil, ErrInvalidOwner
	}
	if opts.Kind != nil {
		return nil, ErrInvalidKind
	}
	if opts.InternalKind == "" {
		return nil, ErrNoInternalKind
	}
	return newEvt(opts)
}

func newEvt(opts *Opts) (*Event, error) {
	updater.start()
	if opts == nil {
		return nil, ErrNoOpts
	}
	if !opts.Target.IsValid() {
		return nil, ErrNoTarget
	}
	var k kind
	if opts.Kind == nil {
		if opts.InternalKind == "" {
			return nil, ErrNoKind
		}
		k.Type = KindTypeInternal
		k.Name = opts.InternalKind
	} else {
		k.Type = KindTypePermission
		k.Name = opts.Kind.FullName()
	}
	var o Owner
	if opts.Owner == nil {
		if opts.RawOwner.Name != "" && opts.RawOwner.Type != "" {
			o = opts.RawOwner
		} else {
			o.Type = OwnerTypeInternal
		}
	} else if opts.Owner.IsAppToken() {
		o.Type = OwnerTypeApp
		o.Name = opts.Owner.GetAppName()
	} else {
		o.Type = OwnerTypeUser
		o.Name = opts.Owner.GetUserName()
	}
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	coll := conn.Events()
	tSpec := getThrottling(&opts.Target, &k)
	if tSpec != nil && tSpec.Max > 0 && tSpec.Time > 0 {
		query := bson.M{
			"target.name":  opts.Target.Name,
			"target.value": opts.Target.Value,
			"starttime":    bson.M{"$gt": time.Now().UTC().Add(-tSpec.Time)},
		}
		if tSpec.KindName != "" {
			query["kind.name"] = tSpec.KindName
		}
		var c int
		c, err = coll.Find(query).Count()
		if err != nil {
			return nil, err
		}
		if c >= tSpec.Max {
			return nil, ErrThrottled{Spec: tSpec, Target: opts.Target}
		}
	}
	now := time.Now().UTC()
	evt := Event{eventData: eventData{
		ID:              eventId{Target: opts.Target},
		UniqueID:        bson.NewObjectId(),
		Target:          opts.Target,
		StartTime:       now,
		Kind:            k,
		Owner:           o,
		StartCustomData: opts.CustomData,
		LockUpdateTime:  now,
		Running:         true,
		Cancelable:      opts.Cancelable,
	}}
	maxRetries := 1
	for i := 0; i < maxRetries+1; i++ {
		err = coll.Insert(evt.eventData)
		if err == nil {
			updater.addCh <- &opts.Target
			return &evt, nil
		}
		if mgo.IsDup(err) {
			if i >= maxRetries || !checkIsExpired(coll, evt.ID) {
				var existing Event
				err = coll.FindId(evt.ID).One(&existing.eventData)
				if err == nil {
					err = ErrEventLocked{event: &existing}
				}
			}
		}
	}
	return nil, err
}

func (e *Event) Abort() error {
	return e.done(nil, nil, true)
}

func (e *Event) Done(evtErr error) error {
	return e.done(evtErr, nil, false)
}

func (e *Event) DoneCustomData(evtErr error, customData interface{}) error {
	return e.done(evtErr, customData, false)
}

func (e *Event) SetLogWriter(w io.Writer) {
	e.logWriter = w
}

func (e *Event) GetLogWriter() io.Writer {
	return &e.logBuffer
}

func (e *Event) SetOtherCustomData(data interface{}) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	return coll.UpdateId(e.ID, bson.M{
		"$set": bson.M{"othercustomdata": data},
	})
}

func (e *Event) Logf(format string, params ...interface{}) {
	log.Debugf(fmt.Sprintf("%s(%s)[%s] %s", e.Target.Name, e.Target.Value, e.Kind, format), params...)
	format += "\n"
	if e.logWriter != nil {
		fmt.Fprintf(e.logWriter, format, params...)
	}
	fmt.Fprintf(&e.logBuffer, format, params...)
}

func (e *Event) TryCancel(reason, owner string) error {
	if !e.Cancelable || !e.Running {
		return ErrNotCancelable
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	change := mgo.Change{
		Update: bson.M{"$set": bson.M{
			"cancelinfo": cancelInfo{
				Owner:     owner,
				Reason:    reason,
				StartTime: time.Now().UTC(),
				Asked:     true,
			},
		}},
		ReturnNew: true,
	}
	_, err = coll.FindId(e.ID).Apply(change, &e.eventData)
	if err == mgo.ErrNotFound {
		return ErrEventNotFound
	}
	return err
}

func (e *Event) AckCancel() error {
	if !e.Cancelable || !e.Running {
		return ErrNotCancelable
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	change := mgo.Change{
		Update: bson.M{"$set": bson.M{
			"cancelinfo.acktime":  time.Now().UTC(),
			"cancelinfo.canceled": true,
		}},
		ReturnNew: true,
	}
	_, err = coll.Find(bson.M{"_id": e.ID, "cancelinfo.asked": true}).Apply(change, &e.eventData)
	if err == mgo.ErrNotFound {
		return ErrEventNotFound
	}
	return err
}

func (e *Event) StartData(value interface{}) error {
	data, err := json.Marshal(e.StartCustomData)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (e *Event) EndData(value interface{}) error {
	data, err := json.Marshal(e.EndCustomData)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (e *Event) OtherData(value interface{}) error {
	data, err := json.Marshal(e.OtherCustomData)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (e *Event) done(evtErr error, customData interface{}, abort bool) (err error) {
	// Done will be usually called in a defer block ignoring errors. This is
	// why we log error messages here.
	defer func() {
		if err != nil {
			log.Errorf("[events] error marking event as done - %#v: %s", e, err)
		}
	}()
	updater.removeCh <- &e.Target
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	coll := conn.Events()
	if abort {
		return coll.RemoveId(e.ID)
	}
	if evtErr != nil {
		e.Error = evtErr.Error()
	} else if e.CancelInfo.Canceled {
		e.Error = "canceled by user request"
	}
	e.EndTime = time.Now().UTC()
	e.EndCustomData = customData
	e.Running = false
	e.Log = e.logBuffer.String()
	var dbEvt Event
	err = coll.FindId(e.ID).One(&dbEvt.eventData)
	if err == nil {
		e.OtherCustomData = dbEvt.OtherCustomData
	}
	defer coll.RemoveId(e.ID)
	e.ID = eventId{ObjId: e.UniqueID}
	return coll.Insert(e.eventData)
}

type lockUpdater struct {
	addCh    chan *Target
	removeCh chan *Target
	stopCh   chan struct{}
	once     *sync.Once
}

func (l *lockUpdater) start() {
	l.once.Do(func() {
		l.stopCh = make(chan struct{})
		go l.spin()
	})
}

func (l *lockUpdater) stop() {
	if l.stopCh == nil {
		return
	}
	l.stopCh <- struct{}{}
	l.stopCh = nil
	l.once = &sync.Once{}
}

func (l *lockUpdater) spin() {
	set := map[Target]struct{}{}
	for {
		select {
		case added := <-l.addCh:
			set[*added] = struct{}{}
		case removed := <-l.removeCh:
			delete(set, *removed)
		case <-l.stopCh:
			return
		case <-time.After(lockUpdateInterval):
		}
		conn, err := db.Conn()
		if err != nil {
			log.Errorf("[events] [lock update] error getting db conn: %s", err)
			continue
		}
		coll := conn.Events()
		slice := make([]interface{}, len(set))
		i := 0
		for id := range set {
			slice[i], _ = id.GetBSON()
			i++
		}
		err = coll.Update(bson.M{"_id": bson.M{"$in": slice}}, bson.M{"$set": bson.M{"lockupdatetime": time.Now().UTC()}})
		if err != nil && err != mgo.ErrNotFound {
			log.Errorf("[events] [lock update] error updating: %s", err)
		}
		conn.Close()
	}
}

func checkIsExpired(coll *storage.Collection, id interface{}) bool {
	var existingEvt Event
	err := coll.FindId(id).One(&existingEvt.eventData)
	if err == nil {
		now := time.Now().UTC()
		lastUpdate := existingEvt.LockUpdateTime.UTC()
		if now.After(lastUpdate.Add(lockExpireTimeout)) {
			existingEvt.Done(fmt.Errorf("event expired, no update for %v", time.Since(lastUpdate)))
			return true
		}
	}
	return false
}
