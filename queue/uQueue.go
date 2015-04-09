package queue

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buaazp/uq/store"
	. "github.com/buaazp/uq/utils"
	"github.com/coreos/go-etcd/etcd"
)

func init() {
	gob.Register(&unitedQueueStore{})
}

const (
	StorageKeyWord   string        = "UnitedQueueKey"
	BgBackupInterval time.Duration = 10 * time.Second
	BgCleanInterval  time.Duration = 20 * time.Second
	BgCleanTimeout   time.Duration = 5 * time.Second
	KeyTopicStore    string        = ":store"
	KeyTopicHead     string        = ":head"
	KeyTopicTail     string        = ":tail"
	KeyLineStore     string        = ":store"
	KeyLineHead      string        = ":head"
	KeyLineRecycle   string        = ":recycle"
	KeyLineInflight  string        = ":inflight"
)

type UnitedQueue struct {
	topics     map[string]*topic
	topicsLock sync.RWMutex
	storage    store.Storage
	etcdLock   sync.RWMutex
	selfAddr   string
	etcdClient *etcd.Client
	etcdKey    string
	etcdStop   chan bool
	wg         sync.WaitGroup
}

type unitedQueueStore struct {
	Topics []string
}

func NewUnitedQueue(storage store.Storage, ip string, port int, etcdServers []string, etcdKey string) (*UnitedQueue, error) {
	topics := make(map[string]*topic)
	etcdStop := make(chan bool)
	uq := new(UnitedQueue)
	uq.topics = topics
	uq.storage = storage
	uq.etcdStop = etcdStop

	if len(etcdServers) > 0 {
		selfAddr := Addrcat(ip, port)
		uq.selfAddr = selfAddr
		etcdClient := etcd.NewClient(etcdServers)
		uq.etcdClient = etcdClient
		uq.etcdKey = etcdKey
	}

	err := uq.loadQueue()
	if err != nil {
		return nil, err
	}

	go uq.etcdRun()
	return uq, nil
}

func (u *UnitedQueue) loadQueue() error {
	unitedQueueStoreData, err := u.storage.Get(StorageKeyWord)
	if err != nil {
		log.Printf("storage not existed: %s", err)
		return nil
	}

	if len(unitedQueueStoreData) > 0 {
		var unitedQueueStoreValue unitedQueueStore
		dec := gob.NewDecoder(bytes.NewBuffer(unitedQueueStoreData))
		if e := dec.Decode(&unitedQueueStoreValue); e == nil {
			for _, topicName := range unitedQueueStoreValue.Topics {
				topicStoreData, err := u.storage.Get(topicName)
				if err != nil || len(topicStoreData) == 0 {
					continue
				}
				var topicStoreValue topicStore
				dec2 := gob.NewDecoder(bytes.NewBuffer(topicStoreData))
				if e := dec2.Decode(&topicStoreValue); e == nil {
					t, err := u.loadTopic(topicName, topicStoreValue)
					if err != nil {
						continue
					}
					u.topicsLock.Lock()
					u.topics[topicName] = t
					u.topicsLock.Unlock()
				}
			}
		}
	}

	log.Printf("united queue load finisded.")
	log.Printf("u.topics: %v", u.topics)
	return nil
}

func (u *UnitedQueue) loadTopic(topicName string, topicStoreValue topicStore) (*topic, error) {
	t := new(topic)
	t.name = topicName
	t.q = u
	t.quit = make(chan bool)

	t.headKey = topicName + KeyTopicHead
	topicHeadData, err := u.storage.Get(t.headKey)
	if err != nil {
		return nil, err
	}
	t.head = binary.LittleEndian.Uint64(topicHeadData)
	t.tailKey = topicName + KeyTopicTail
	topicTailData, err := u.storage.Get(t.tailKey)
	if err != nil {
		return nil, err
	}
	t.tail = binary.LittleEndian.Uint64(topicTailData)

	lines := make(map[string]*line)
	for _, lineName := range topicStoreValue.Lines {
		lineStoreKey := topicName + "/" + lineName
		lineStoreData, err := u.storage.Get(lineStoreKey)
		if err != nil || len(lineStoreData) == 0 {
			continue
		}
		var lineStoreValue lineStore
		dec3 := gob.NewDecoder(bytes.NewBuffer(lineStoreData))
		if e := dec3.Decode(&lineStoreValue); e == nil {
			l, err := t.loadLine(lineName, lineStoreValue)
			if err != nil {
				continue
			}
			lines[lineName] = l
			log.Printf("line: %v", l)
			log.Printf("line[%s] load succ.", lineStoreKey)
		}
	}
	t.lines = lines

	err = u.registerTopic(t.name)
	if err != nil {
		log.Printf("register topic error: %s", err)
	}

	t.start()
	log.Printf("topic[%s] load succ.", topicName)
	log.Printf("topic: %v", t)
	return t, nil
}

func (u *UnitedQueue) Create(qr *QueueRequest) error {
	if qr.TopicName == "" {
		return NewError(
			ErrBadKey,
			`create topic name is nil`,
		)
	}

	var err error
	if qr.LineName != "" {
		u.topicsLock.RLock()
		t, ok := u.topics[qr.TopicName]
		u.topicsLock.RUnlock()
		if !ok {
			return NewError(
				ErrTopicNotExisted,
				`queue create`,
			)
		}

		err = t.createLine(qr.LineName, qr.Recycle, false)
		if err != nil {
			log.Printf("create line[%s] error: %s", qr.LineName, err)
		}
	} else {
		err = u.createTopic(qr.TopicName, false)
		if err != nil {
			log.Printf("create topic[%s] error: %s", qr.TopicName, err)
		}
	}

	return err
}

func (u *UnitedQueue) createTopic(name string, fromEtcd bool) error {
	u.topicsLock.RLock()
	_, ok := u.topics[name]
	u.topicsLock.RUnlock()
	if ok {
		return NewError(
			ErrTopicExisted,
			`queue createTopic`,
		)
	}

	t, err := u.newTopic(name)
	if err != nil {
		return err
	}

	u.topicsLock.Lock()
	u.topics[name] = t
	u.topicsLock.Unlock()

	err = u.exportQueue()
	if err != nil {
		delete(u.topics, name)
		return err
	}

	if !fromEtcd {
		err = u.registerTopic(t.name)
		if err != nil {
			log.Printf("register topic error: %s", err)
		}
	}
	log.Printf("topic[%s] created.", name)
	return nil
}

func (u *UnitedQueue) newTopic(name string) (*topic, error) {
	lines := make(map[string]*line)
	t := new(topic)
	t.name = name
	t.lines = lines
	t.head = 0
	t.headKey = name + KeyTopicHead
	t.tail = 0
	t.tailKey = name + KeyTopicTail
	t.q = u
	t.quit = make(chan bool)

	err := t.exportHead()
	if err != nil {
		return nil, err
	}
	err = t.exportTail()
	if err != nil {
		return nil, err
	}

	t.start()
	return t, nil
}

func (u *UnitedQueue) exportQueue() error {
	log.Printf("start export queue...")

	queueStoreValue, err := u.genQueueStore()
	if err != nil {
		return err
	}

	buffer := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buffer)
	err = enc.Encode(queueStoreValue)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}

	err = u.storage.Set(StorageKeyWord, buffer.Bytes())
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}

	log.Printf("united queue export finisded.")
	return nil
}

func (u *UnitedQueue) genQueueStore() (*unitedQueueStore, error) {
	u.topicsLock.RLock()
	defer u.topicsLock.RUnlock()

	topics := make([]string, len(u.topics))
	i := 0
	for topicName, _ := range u.topics {
		topics[i] = topicName
		i++
	}

	qs := new(unitedQueueStore)
	qs.Topics = topics
	return qs, nil
}

func (u *UnitedQueue) Push(name string, data []byte) error {
	u.topicsLock.RLock()
	t, ok := u.topics[name]
	u.topicsLock.RUnlock()
	if !ok {
		return NewError(
			ErrTopicNotExisted,
			`queue push`,
		)
	}

	return t.push(data)
}

func (u *UnitedQueue) MultiPush(name string, datas [][]byte) error {
	u.topicsLock.RLock()
	t, ok := u.topics[name]
	u.topicsLock.RUnlock()
	if !ok {
		return NewError(
			ErrTopicNotExisted,
			`queue multiPush`,
		)
	}

	return t.mPush(datas)
}

func (u *UnitedQueue) Pop(name string) (uint64, []byte, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return 0, nil, NewError(
			ErrBadKey,
			`pop key parts error: `+ItoaQuick(len(parts)),
		)
	}

	tName := parts[0]
	lName := parts[1]

	u.topicsLock.RLock()
	t, ok := u.topics[tName]
	u.topicsLock.RUnlock()
	if !ok {
		log.Printf("topic[%s] not existed.", tName)
		return 0, nil, NewError(
			ErrTopicNotExisted,
			`queue pop`,
		)
	}

	return t.pop(lName)
}

func (u *UnitedQueue) MultiPop(name string, n int) ([]uint64, [][]byte, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return nil, nil, NewError(
			ErrBadKey,
			`mPop key parts error: `+ItoaQuick(len(parts)),
		)
	}

	tName := parts[0]
	lName := parts[1]

	u.topicsLock.RLock()
	t, ok := u.topics[tName]
	u.topicsLock.RUnlock()
	if !ok {
		log.Printf("topic[%s] not existed.", tName)
		return nil, nil, NewError(
			ErrTopicNotExisted,
			`queue multiPop`,
		)
	}

	return t.mPop(lName, n)
}

func (u *UnitedQueue) Confirm(key string) error {
	var topicName, lineName string
	var id uint64
	var err error
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return NewError(
			ErrBadKey,
			`confirm key parts error: `+ItoaQuick(len(parts)),
		)
	} else {
		topicName = parts[0]
		lineName = parts[1]
		id, err = strconv.ParseUint(parts[2], 10, 0)
		if err != nil {
			return NewError(
				ErrBadKey,
				`confirm key parse id error: `+err.Error(),
			)
		}
	}

	u.topicsLock.RLock()
	t, ok := u.topics[topicName]
	u.topicsLock.RUnlock()
	if !ok {
		log.Printf("topic[%s] not existed.", topicName)
		return NewError(
			ErrTopicNotExisted,
			`queue confirm`,
		)
	}

	return t.confirm(lineName, id)
}

func (u *UnitedQueue) MultiConfirm(name string, ids []uint64) (int, error) {
	var topicName, lineName string
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return 0, NewError(
			ErrBadKey,
			`mConfirm key parts error: `+ItoaQuick(len(parts)),
		)
	} else {
		topicName = parts[0]
		lineName = parts[1]
	}

	u.topicsLock.RLock()
	t, ok := u.topics[topicName]
	u.topicsLock.RUnlock()
	if !ok {
		log.Printf("topic[%s] not existed.", topicName)
		return 0, NewError(
			ErrTopicNotExisted,
			`queue multiConfirm`,
		)
	}

	return t.mConfirm(lineName, ids)
}

func (u *UnitedQueue) setData(key string, data []byte) error {
	err := u.storage.Set(key, data)
	if err != nil {
		log.Printf("key[%s] set data error: %s", key, err)
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (u *UnitedQueue) getData(key string) ([]byte, error) {
	data, err := u.storage.Get(key)
	if err != nil {
		log.Printf("key[%s] get data error: %s", key, err)
		return nil, NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return data, nil
}

func (u *UnitedQueue) delData(key string) error {
	err := u.storage.Del(key)
	if err != nil {
		log.Printf("key[%s] del data error: %s", key, err)
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (u *UnitedQueue) Close() {
	log.Printf("uq stoping...")
	close(u.etcdStop)
	u.wg.Wait()

	for _, t := range u.topics {
		t.close()
	}

	err := u.exportTopics()
	if err != nil {
		log.Printf("export queue error: %s", err)
	}

	u.storage.Close()
}

func (u *UnitedQueue) exportTopics() error {
	u.topicsLock.RLock()
	defer u.topicsLock.RUnlock()

	for _, t := range u.topics {
		err := t.exportLines()
		if err != nil {
			log.Printf("topic[%s] export lines error: %s", t.name, err)
			continue
		}
		err = t.exportTopic()
		if err != nil {
			log.Printf("topic[%s] export error: %s", t.name, err)
			continue
		}
	}

	log.Printf("export all topics succ.")
	return nil
}

func (u *UnitedQueue) Empty(qr *QueueRequest) error {
	if qr.TopicName == "" {
		return NewError(
			ErrBadKey,
			`empty topic is nil`,
		)
	}

	var err error
	if qr.LineName != "" {
		u.topicsLock.RLock()
		t, ok := u.topics[qr.TopicName]
		u.topicsLock.RUnlock()
		if !ok {
			return NewError(
				ErrTopicNotExisted,
				`queue empty`,
			)
		}

		err = t.emptyLine(qr.LineName)
		if err != nil {
			log.Printf("empty line[%s] error: %s", qr.LineName, err)
		}
	} else {
		err = u.emptyTopic(qr.TopicName)
		if err != nil {
			log.Printf("empty topic[%s] error: %s", qr.TopicName, err)
		}
	}

	return err
}

func (u *UnitedQueue) emptyTopic(name string) error {
	u.topicsLock.RLock()
	t, ok := u.topics[name]
	u.topicsLock.RUnlock()
	if !ok {
		return NewError(
			ErrTopicNotExisted,
			`queue emptyTopic`,
		)
	}

	return t.empty()
}
