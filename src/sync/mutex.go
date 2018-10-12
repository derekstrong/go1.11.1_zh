// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

//声明throw函数； runtime/panic.go  : 突破访问限制 可以在sync中使用throw这个runtime中的私有函数
func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32   // 将一个32位整数拆分为 当前阻塞的goroutine数(29位)|饥饿状态(1位)|唤醒状态(1位)|锁状态(1位) 的形式，来简化字段设计
	sema  uint32  // 信号量  pv 操作
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

//互斥量可分为两种操作模式:正常和饥饿。
//在正常模式下，等待的goroutines按照FIFO（先进先出）顺序排队，但是goroutine被唤醒之后并不能立即得到mutex锁，它需要与新到达的goroutine争夺mutex锁。
//因为新到达的goroutine已经在CPU上运行了，所以被唤醒的goroutine很大概率是争夺mutex锁是失败的。出现这样的情况时候，被唤醒的goroutine需要排队在队列的前面。
//如果被唤醒的goroutine有超过1ms没有获取到mutex锁，那么它就会变为饥饿模式。
//在饥饿模式中，mutex锁直接从解锁的goroutine交给队列前面的goroutine。新达到的goroutine也不会去争夺mutex锁（即使没有锁，也不能去自旋），而是到等待队列尾部排队。
//在饥饿模式下，有一个goroutine获取到mutex锁了，如果它满足下条件中的任意一个，mutex将会切换回去正常模式：
//  1. 是等待队列中的最后一个goroutine
//  2. 它的等待时间不超过1ms。
//正常模式有更好的性能，因为goroutine可以连续多次获得mutex锁；
//饥饿模式对于预防队列尾部goroutine一致无法获取mutex锁的问题。

const (
	mutexLocked = 1 << iota // mutex is locked  0001 含义：用最后一位表示当前对象锁的状态，0-未锁住 1-已锁住
	mutexWoken    // 2 0010 含义：用倒数第二位表示当前对象是否被唤醒 0-未唤醒 1-唤醒
	mutexStarving  // 4 0100 含义：用倒数第三位表示当前对象是否为饥饿模式，0为正常模式，1为饥饿模式。
	mutexWaiterShift = iota   // 3，从倒数第四位往前的bit位表示在排队等待的goroutine数


	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6 //  等待时间阀值，超过就转成饥饿模式
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
// 阻塞调用 直到获取到这个mutex(m.state 最后一位 由 0 ==> 1) 主要是通过CAS来实现无锁的竞争
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 先尝试抢占，如果m.state是0 表明当前Mutex是未上锁 则直接锁住 然后退出 完成Lock操作
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {  // 是否开启 -race 检查  可忽略
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	var waitStartTime int64  // 等待起始时间
	starving := false 	     // 饥饿模式标识
	awoke := false           // 唤醒标识
	iter := 0                // 自旋累计尝试次数
	old := m.state           // 记录当前的状态 为后面的CAS做准备
	for {  // CAS 尝试
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 如果当前状态是已经被锁住且不是饥饿模式 并且 是可以自旋的 (自旋是有尝试次数的) 则进入自旋的方式进行竞争Mutex
		// 自旋锁 获取锁失败后不会引起调用者睡眠，会一直占用CPU，无上下文切换的开销，速度会很快， 但是避免长时间处于自旋状态 busy-waiting
		// 互斥锁 获取锁失败后当前线程会被阻塞 sleep-waiting
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.  主动旋转是有意义的。
			// Try to set mutexWoken flag to inform Unlock  试着设置mutexwake标志到解锁
			// to not wake other blocked goroutines.  不要唤醒其他阻塞的goroutines

			// 如果Mutex当前的状态是未唤醒 并且还有阻塞的goroutine
			// 则尝试将唤醒状态设置成已唤醒，如果尝试成功则将唤醒操作标识设置为true，避免多次自旋重复操作
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
				// MyTODO: 是否不需要再进行后面的自旋操作了？ 在进入spin的if判断中是否要判断一下
				// (加一个 && 判断条件：!awoke ? 不行， 下次尝试的时候会将当前goroutine的awoke设置成true)
			}
			runtime_doSpin() // 进入自旋锁后当前goroutine并不挂起，仍然在占用cpu资源，所以重试一定次数后，不会再进入自旋锁逻辑
			iter++ // 自旋次数加一
			old = m.state // 保存当前状态，为下一次的CAS做准备
			continue
		}  // 自旋操作结束

		new := old // 保存当前状态，主要为了后面增加阻塞队列个数的CAS尝试
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {   // 如果当前状态是正常模式
			new |= mutexLocked        // 确保Mutex是已锁住的状态
		}
		if old&(mutexLocked|mutexStarving) != 0 { // 如果当前状态是已锁住 或者是 饥饿模式
			new += 1 << mutexWaiterShift          // 则将新来的goroutine排队 将等待goroutine数+1
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 切换饥饿模式的时候 要看判断一下当前是否已经解锁了，如果已经解锁了就不要切换
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving  // 确保当前状态是饥饿模式
		}

		if awoke { // goroutine已经被唤醒, 我们需要重新设置状态位
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// 如果状态的唤醒标志位是0 则与awoke不一致 需要抛出异常
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 位清空运算
			// (^mutexWoken) => xxxx...xxxx & (^0010) => xxxx...xxxx & 1101 = xxxx...xx0x ：设置唤醒状态为0,
			// 唤醒后操作只能被一个goroutine感知 通过后面的CAS来保证
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				// 如果之前的状态是没有锁住并且是正常状态，则直接跳出，完成Lock操作，此时状态已经通过CAS改成new：已经置成已锁住的状态
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo)  // PV 操作的 P 来竞争信号量sema， 其中通过queueLife这个bool值来进行排队优化
			// 判断是否是饥饿模式 : 当前已经是饥饿模式 或者这个goroutine等待时间操作 starvationThresholdNs(1ms) 这个阈值的设置
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state // 记录当前的状态，为后面CAS做准备
			if old&mutexStarving != 0 {  //如果当前状态是饥饿模式
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 如果当前的mutex的状态是 未锁住或者未唤醒 或者 等待阻塞的goroutine数为0 则是状态不一致的情况，需要抛异常
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 状态锁住 & 等待goroutine数减1
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// 退出饥饿模式的条件： 如果当前模式标识是非饥饿模式 或者是goroutine等待队列中只有一个
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta) // 原子操作： 锁获取成功，更新状态
				break // Lock() 操作结束 当前goroutine获取锁
			}
			awoke = true // 将唤醒标识设置成true, 本次尝试获取Lock未成功，尝试下一次的Lock
			iter = 0 // 重置自旋起始次数
		} else { // CAS 失败，更新当前的状态 作为下次CAS的验证前提
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
// 如果对一个没有上锁的mutex进行解锁，会抛出运行时异常
// 一个锁住的mutex不属于某一个g, 可以在一个g中上锁然后在另一个g中解锁
func (m *Mutex) Unlock() {
	if race.Enabled { //开始竞争检测的逻辑
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit. 释放锁
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if (new+mutexLocked)&mutexLocked == 0 { // 如果对没有锁住的mutex进行unlock，则抛出异常
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 { //正常模式
		old := new //为CAS做准备
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待的g 或者 mutex的状态不是已经锁住且已经唤醒且状态是饥饿模式 则不需要进行任何操作 直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken // 等待g数减1 并且将是否唤醒状态支撑已经唤醒
			if atomic.CompareAndSwapInt32(&m.state, old, new) { // CAS
				runtime_Semrelease(&m.sema, false) // 信号量 sema 的V操作。尝试进行释放信号量
				return // 完成Unlock
			}
			old = m.state // 为下一次CAS做准备
		}
	} else {
		// 饥饿模式:将mutex所有权移交给下一个等待的goroutine
		// 注意:mutexlock没有设置，goroutine会在唤醒后设置。
		// 当饥饿模式被设置，互斥锁仍然被认为是锁定的，新来的goroutines不会得到它 -- 饥饿模式保证只给下一个等待的g
		// Starving mode: handoff mutex ownership to the next waiter.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true) // 信号量 sema 的V操作。进行释放等待队列的下一个g
	}
}
