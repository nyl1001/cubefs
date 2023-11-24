package rdma

import "C"
import (
	"sync"
	"syscall"
)

var (
	epollFd = -1
	lock    sync.Mutex
	once    sync.Once
)

type ReadAble func()
type EpollContext struct {
	cond *sync.Cond
}

type EPoll struct {
	epollFd int
	fds     map[int]ReadAble
}

var instance *EPoll

func GetEpoll() *EPoll {
	once.Do(func() {
		instance = &EPoll{}
		instance.init()
	})

	return instance
}

func (e *EPoll) init() {
	var err error
	e.epollFd, err = syscall.EpollCreate(256)
	if err != nil {
		panic(err)
	}
	instance.fds = make(map[int]ReadAble)

	go e.epollLoop()
}

func (e *EPoll) getContext(fd int) ReadAble {
	return e.fds[fd]
}

func (e *EPoll) EpollAdd(fd int, ctx ReadAble) {
	event := syscall.EpollEvent{}
	event.Events = syscall.EPOLLIN
	event.Fd = int32(fd)
	lock.Lock()
	e.fds[fd] = ctx
	lock.Unlock()
	syscall.EpollCtl(e.epollFd, syscall.EPOLL_CTL_ADD, fd, &event)
}

func (e *EPoll) EpollDel(fd int) {
	//println(fd)
	lock.Lock()
	delete(e.fds, fd)
	lock.Unlock()
	syscall.EpollCtl(e.epollFd, syscall.EPOLL_CTL_DEL, fd, nil)
}

func (e *EPoll) epollLoop() error {
	for {
		events := make([]syscall.EpollEvent, 100)
		n, err := syscall.EpollWait(e.epollFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		//println(n)
		for i := 0; i < n; i++ {
			//print(i)
			//print("->")
			//println(n)
			print("event.Fd")
			println(int(events[i].Fd))
			e.getContext(int(events[i].Fd))()
			//go e.getContext(int(events[i].Fd))()
		}
	}
}
