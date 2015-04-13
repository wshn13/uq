package queue

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"encoding/gob"
	"log"
	"sync"
	"time"

	. "github.com/buaazp/uq/utils"
)

func init() {
	gob.Register(&topicStore{})
}

type topic struct {
	name      string
	lines     map[string]*line
	linesLock sync.RWMutex
	head      uint64
	headLock  sync.RWMutex
	headKey   string
	tail      uint64
	tailLock  sync.RWMutex
	tailKey   string
	q         *UnitedQueue

	quit chan bool
	wg   sync.WaitGroup
}

type topicStore struct {
	Lines []string
}

func (t *topic) start() {
	// log.Printf("topic[%s] is starting...", t.name)
	go t.backgroundClean()
}

func (t *topic) createLine(name string, recycle time.Duration, fromEtcd bool) error {
	t.linesLock.Lock()
	defer t.linesLock.Unlock()
	_, ok := t.lines[name]
	if ok {
		return NewError(
			ErrLineExisted,
			`topic createLine`,
		)
	}

	l, err := t.newLine(name, recycle)
	if err != nil {
		return err
	}

	if !fromEtcd {
		t.q.registerLine(t.name, l.name, l.recycle.String())
		// err := t.q.registerLine(t.name, l.name, l.recycle.String())
		// if err != nil {
		// 	log.Printf("topic[%s] register line error: %s", t.name, err)
		// }
	}

	t.lines[name] = l

	err = t.exportTopic()
	if err != nil {
		t.linesLock.Lock()
		delete(t.lines, name)
		t.linesLock.Unlock()
		return err
	}

	log.Printf("topic[%s] line[%s:%v] created.", t.name, name, recycle)
	return nil
}

func (t *topic) newLine(name string, recycle time.Duration) (*line, error) {
	inflight := list.New()
	imap := make(map[uint64]bool)
	l := new(line)
	l.name = name
	l.head = t.head
	l.recycle = recycle
	l.recycleKey = t.name + "/" + name + KeyLineRecycle
	l.inflight = inflight
	l.ihead = t.head
	l.imap = imap
	l.t = t

	err := l.exportLine()
	if err != nil {
		return nil, err
	}
	err = l.exportRecycle()
	if err != nil {
		return nil, err
	}

	return l, nil
}

func (t *topic) getHead() uint64 {
	t.headLock.RLock()
	defer t.headLock.RUnlock()
	return t.head
}

func (t *topic) getTail() uint64 {
	t.tailLock.RLock()
	defer t.tailLock.RUnlock()
	return t.tail
}

func (t *topic) exportHead() error {
	topicHeadData := make([]byte, 8)
	binary.LittleEndian.PutUint64(topicHeadData, t.head)
	err := t.q.setData(t.headKey, topicHeadData)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (t *topic) removeHeadData() error {
	err := t.q.delData(t.headKey)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (t *topic) exportTail() error {
	topicTailData := make([]byte, 8)
	binary.LittleEndian.PutUint64(topicTailData, t.tail)
	err := t.q.setData(t.tailKey, topicTailData)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (t *topic) removeTailData() error {
	err := t.q.delData(t.tailKey)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (t *topic) exportTopic() error {
	topicStoreValue, err := t.genTopicStore()
	if err != nil {
		return err
	}

	buffer := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buffer)
	err = enc.Encode(topicStoreValue)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}

	err = t.q.setData(t.name, buffer.Bytes())
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}

	// log.Printf("topic[%s] export finisded.", t.name)
	return nil
}

func (t *topic) removeTopicData() error {
	err := t.q.delData(t.name)
	if err != nil {
		return NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	return nil
}

func (t *topic) genTopicStore() (*topicStore, error) {
	lines := make([]string, len(t.lines))
	i := 0
	for _, line := range t.lines {
		lines[i] = line.name
		i++
	}

	ts := new(topicStore)
	ts.Lines = lines

	return ts, nil
}

func (t *topic) loadLine(lineName string, lineStoreValue lineStore) (*line, error) {
	// log.Printf("topic[%s] loading inflights: %v", t.name, lineStoreValue.Inflights)
	l := new(line)
	l.name = lineName
	l.recycleKey = t.name + "/" + lineName + KeyLineRecycle
	lineRecycleData, err := t.q.getData(l.recycleKey)
	if err != nil {
		return nil, NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	lineRecycle, err := time.ParseDuration(string(lineRecycleData))
	if err != nil {
		return nil, NewError(
			ErrInternalError,
			err.Error(),
		)
	}
	l.recycle = lineRecycle
	l.head = lineStoreValue.Head
	l.ihead = lineStoreValue.Ihead
	imap := make(map[uint64]bool)
	for i := l.ihead; i < l.head; i++ {
		imap[i] = false
	}
	l.imap = imap
	inflight := list.New()
	for index, _ := range lineStoreValue.Inflights {
		msg := &lineStoreValue.Inflights[index]
		inflight.PushBack(msg)
		imap[msg.Tid] = true
	}
	l.inflight = inflight
	l.t = t

	t.q.registerLine(t.name, l.name, l.recycle.String())
	// err = t.q.registerLine(t.name, l.name, l.recycle.String())
	// if err != nil {
	// 	log.Printf("topic[%s] register line error: %s", t.name, err)
	// }
	return l, nil
}

func (t *topic) push(data []byte) error {
	t.tailLock.Lock()
	defer t.tailLock.Unlock()

	key := Acatui(t.name, ":", t.tail)
	err := t.q.setData(key, data)
	if err != nil {
		return err
	}
	// log.Printf("topic[%s] %s pushed.", t.name, string(data))

	t.tail++
	err = t.exportTail()
	if err != nil {
		t.tail--
		return err
	}

	return nil
}

func (t *topic) mPush(datas [][]byte) error {
	t.tailLock.Lock()
	defer t.tailLock.Unlock()

	oldTail := t.tail
	for _, data := range datas {
		if len(data) <= 0 {
			return NewError(
				ErrBadRequest,
				`message has no content`,
			)
		}

		key := Acatui(t.name, ":", t.tail)
		err := t.q.setData(key, data)
		if err != nil {
			t.tail = oldTail
			return err
		}
		// log.Printf("topic[%s] %s pushed.", t.name, string(data))
		t.tail++
	}

	err := t.exportTail()
	if err != nil {
		t.tail = oldTail
		return err
	}

	return nil
}

func (t *topic) pop(name string) (uint64, []byte, error) {
	t.linesLock.RLock()
	l, ok := t.lines[name]
	t.linesLock.RUnlock()
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return 0, nil, NewError(
			ErrLineNotExisted,
			`topic pop`,
		)
	}

	return l.pop()
}

func (t *topic) mPop(name string, n int) ([]uint64, [][]byte, error) {
	t.linesLock.RLock()
	l, ok := t.lines[name]
	t.linesLock.RUnlock()
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return nil, nil, NewError(
			ErrLineNotExisted,
			`topic mPop`,
		)
	}

	return l.mPop(n)
}

func (t *topic) confirm(name string, id uint64) error {
	t.linesLock.RLock()
	l, ok := t.lines[name]
	t.linesLock.RUnlock()
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return NewError(
			ErrLineNotExisted,
			`topic confirm`,
		)
	}

	return l.confirm(id)
}

func (t *topic) emptyLine(name string) error {
	t.linesLock.RLock()
	l, ok := t.lines[name]
	t.linesLock.RUnlock()
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return NewError(
			ErrLineNotExisted,
			`topic emptyLine`,
		)
	}

	return l.empty()
}

func (t *topic) empty() error {
	t.linesLock.RLock()
	defer t.linesLock.RUnlock()

	for _, l := range t.lines {
		err := l.empty()
		if err != nil {
			// log.Printf("topic[%s] line[%s] empty error: %s", t.name, name, err)
			return err
		}
	}

	t.headLock.Lock()
	defer t.headLock.Unlock()
	t.tailLock.RLock()
	defer t.tailLock.RUnlock()
	t.head = t.tail
	err := t.exportHead()
	if err != nil {
		return err
	}

	log.Printf("topic[%s] empty succ", t.name)
	return nil
}

func (t *topic) removeLine(name string, fromEtcd bool) error {
	t.linesLock.RLock()
	defer t.linesLock.Unlock()
	l, ok := t.lines[name]
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return NewError(
			ErrLineNotExisted,
			`topic statLine`,
		)
	}

	if !fromEtcd {
		err := t.q.unRegisterLine(t.name, name)
		if err != nil {
			return err
		}
	}

	delete(t.lines, name)
	err := t.exportTopic()
	if err != nil {
		t.linesLock.Lock()
		t.lines[name] = l
		t.linesLock.Unlock()
		return err
	}

	return l.remove()
}

func (t *topic) removeLines() error {
	for lineName, l := range t.lines {
		err := l.remove()
		if err != nil {
			log.Printf("topic[%s] line[%s] remove error: %s", t.name, lineName, err)
			continue
		}
		delete(t.lines, lineName)
	}

	// log.Printf("topic[%s]'s all lines removed.", t.name)
	return nil
}

func (t *topic) remove() error {
	t.close()

	t.linesLock.Lock()
	defer t.linesLock.Unlock()

	err := t.removeLines()
	if err != nil {
		log.Printf("topic[%s] removeLines error: %s", err)
	}

	t.headLock.Lock()
	defer t.headLock.Unlock()
	err = t.removeHeadData()
	if err != nil {
		log.Printf("topic[%s] removeHeadData error: %s", err)
	}

	t.tailLock.Lock()
	defer t.tailLock.Unlock()
	err = t.removeTailData()
	if err != nil {
		log.Printf("topic[%s] removeTailData error: %s", err)
	}

	err = t.removeTopicData()
	if err != nil {
		log.Printf("topic[%s] removeTopicData error: %s", err)
	}

	err = t.removeMsgData()
	if err != nil {
		log.Printf("topic[%s] removeMsgData error: %s", err)
	}

	return nil
}

func (t *topic) removeMsgData() error {
	for i := t.head; i < t.tail; i++ {
		key := Acatui(t.name, ":", i)
		err := t.q.delData(key)
		if err != nil {
			log.Printf("topic[%s] del data[%s] error; %s", t.name, key, err)
			continue
		}
	}
	return nil
}

func (t *topic) statLine(name string) (*QueueStat, error) {
	t.linesLock.RLock()
	l, ok := t.lines[name]
	t.linesLock.RUnlock()
	if !ok {
		// log.Printf("topic[%s] line[%s] not existed.", t.name, name)
		return nil, NewError(
			ErrLineNotExisted,
			`topic statLine`,
		)
	}

	return l.stat()
}

func (t *topic) stat() (*QueueStat, error) {
	qs := new(QueueStat)
	qs.Name = t.name
	qs.Type = "topic"

	t.linesLock.RLock()
	qs.Lines = make([]*QueueStat, 0)
	for name, l := range t.lines {
		//TODO: print lines
		ls, err := l.stat()
		if err != nil {
			log.Printf("topic[%s] lien[%s] stat error: %s", t.name, name, err)
			continue
		}
		qs.Lines = append(qs.Lines, ls)
	}
	t.linesLock.RUnlock()

	t.headLock.RLock()
	qs.Head = t.head
	t.headLock.RUnlock()

	t.tailLock.RLock()
	qs.Tail = t.tail
	t.tailLock.RUnlock()

	qs.Count = qs.Tail - qs.Head

	return qs, nil
}

func (t *topic) getData(id uint64) ([]byte, error) {
	key := Acatui(t.name, ":", id)
	return t.q.getData(key)
}

func (t *topic) backgroundClean() {
	t.wg.Add(1)
	defer t.wg.Done()

	bgQuit := false
	backupTick := time.NewTicker(BgBackupInterval)
	cleanTick := time.NewTicker(BgCleanInterval)
	for !bgQuit {
		select {
		case <-backupTick.C:
			err := t.exportLines()
			if err != nil {
				log.Printf("topic[%s] export lines error: %s", t.name, err)
			}
		case <-cleanTick.C:
			bgQuit := t.clean()
			if bgQuit {
				// log.Printf("topic[%s] t.clean return quit: %v", t.name, bgQuit)
				break
			}
		case <-t.quit:
			// log.Printf("topic[%s] background clean catched quit", t.name)
			bgQuit = true
			break
		}
	}
	// log.Printf("topic[%s] background clean exit.", t.name)
}

func (t *topic) exportLines() error {
	t.linesLock.RLock()
	defer t.linesLock.RUnlock()

	for lineName, l := range t.lines {
		l.inflightLock.RLock()
		l.headLock.RLock()
		err := l.exportLine()
		l.inflightLock.RUnlock()
		l.headLock.RUnlock()
		if err != nil {
			log.Printf("topic[%s] line[%s] export error: %s", t.name, lineName, err)
			continue
		}
	}

	// log.Printf("topic[%s]'s all lines exported.", t.name)
	return nil
}

func (t *topic) clean() (quit bool) {
	quit = false

	t.headLock.Lock()
	defer t.headLock.Unlock()

	// starting := t.head
	endTime := time.Now().Add(BgCleanTimeout)
	// log.Printf("topic[%s] begin to clean at %d", t.name, starting)

	// defer func() {
	// 	if t.head != starting {
	// 		log.Printf("topic[%s] garbage[%d - %d] are cleaned", t.name, starting, t.head-1)
	// 	}
	// }()

	ending := t.getEnd()
	for t.head < ending {
		select {
		case <-t.quit:
			quit = true
			// log.Printf("topic[%s] catched quit at %d", t.name, t.head)
			return
		default:
			// nothing todo
		}

		if time.Now().After(endTime) {
			// log.Printf("topic[%s] cleaning timeout, break at %d", t.name, t.head)
			return
		}

		key := Acatui(t.name, ":", t.head)
		err := t.q.delData(key)
		if err != nil {
			// log.Printf("topic[%s] del data[%s] error; %s", t.name, key, err)
			return
		}

		t.head++
		err = t.exportHead()
		if err != nil {
			log.Printf("topic[%s] export head error: %s", t.name, err)
			return
		}
	}

	return
}

func (t *topic) getEnd() uint64 {
	var end uint64
	if len(t.lines) == 0 {
		end = t.head
	} else {
		end = t.tail
		for _, l := range t.lines {
			if l.recycle > 0 {
				if l.ihead < end {
					end = l.ihead
				}
			} else {
				if l.head < end {
					end = l.head
				}
			}
		}
	}
	return end
}

func (t *topic) close() {
	close(t.quit)
	t.wg.Wait()
}
