package buf

import (
	"fmt"
	"github.com/chubaofs/chubaofs/util"
	"github.com/chubaofs/chubaofs/util/log"
	"sync"
	"sync/atomic"
	"time"
)



// BufferPool defines the struct of a buffered pool with 4 objects.
type BufferPool struct {
	pools                  [2]*sync.Pool
	tinyPool               *sync.Pool
	blockSizeGetNum        uint64
	avaliBlockSizePutNum   uint64
	unavaliBlockSizePutNum uint64
}

// NewBufferPool returns a new buffered pool.
func NewBufferPool() (bufferP *BufferPool) {
	bufferP = &BufferPool{}
	bufferP.pools[0] = &sync.Pool{
		New: func() interface{}{
			return make([]byte, util.PacketHeaderSize)
		},
	}
	bufferP.pools[1] = &sync.Pool{
		New: func() interface{}{
			return make([]byte, util.BlockSize)
		},
	}
	bufferP.tinyPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, util.DefaultTinySizeLimit)
		},
	}
	go bufferP.LogToBlockSizeGetOrPutDetail()
	return bufferP
}



// Get returns the data based on the given size. Different size corresponds to different object in the pool.
func (bufferP *BufferPool) Get(size int) (data []byte, err error) {
	if size == util.PacketHeaderSize {
		return bufferP.pools[0].Get().([]byte), nil
	} else if size == util.BlockSize {
		atomic.AddUint64(&bufferP.blockSizeGetNum,1)
		return bufferP.pools[1].Get().([]byte), nil
	} else if size == util.DefaultTinySizeLimit {
		return bufferP.tinyPool.Get().([]byte), nil
	}
	return nil, fmt.Errorf("can only support 45 or 65536 bytes")
}

func (bufferP *BufferPool)LogToBlockSizeGetOrPutDetail(){
	for {
		getNum:=atomic.LoadUint64(&bufferP.blockSizeGetNum)
		avaliPutNum:=atomic.LoadUint64(&bufferP.avaliBlockSizePutNum)
		unavaliPutNum:=atomic.LoadUint64(&bufferP.unavaliBlockSizePutNum)
		log.LogErrorf(fmt.Sprintf("LogToBlockSizeGetOrPutDetail getNum(%v),avaliputNum(%v)" +
			" unavaliputNum(%v)",getNum,avaliPutNum,unavaliPutNum))
		time.Sleep(time.Second*60)
	}

}

// Put puts the given data into the buffer pool.
func (bufferP *BufferPool) Put(data []byte) {
	if data == nil {
		atomic.AddUint64(&bufferP.unavaliBlockSizePutNum,1)
		return
	}
	size := len(data)
	if size == util.PacketHeaderSize {
		bufferP.pools[0].Put(data)
	} else if size == util.BlockSize {
		atomic.AddUint64(&bufferP.avaliBlockSizePutNum,1)
		bufferP.pools[1].Put(data)
	} else if size == util.DefaultTinySizeLimit {
		bufferP.tinyPool.Put(data)
	}else {
		atomic.AddUint64(&bufferP.unavaliBlockSizePutNum,1)
	}
	return
}

