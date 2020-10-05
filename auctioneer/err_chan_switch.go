package auctioneer

import "sync"

// ErrChanSwitch is a type that can switch incoming error messages between a
// main channel and a temporary channel in a concurrency safe way.
type ErrChanSwitch struct {
	mainChan chan<- error
	tempChan chan<- error
	diverted bool

	incomingChan chan error

	sync.Mutex
	wg sync.WaitGroup

	quit chan struct{}
}

// NewErrChanSwitch creates a new error channel switcher with the given main
// channel that error messages are forwarded to by default.
func NewErrChanSwitch(mainChan chan<- error) *ErrChanSwitch {
	return &ErrChanSwitch{
		mainChan:     mainChan,
		diverted:     false,
		incomingChan: make(chan error),
		quit:         make(chan struct{}),
	}
}

// ErrChan returns the incoming channel that errors can be sent to that are then
// switched in a concurrency safe way.
func (s *ErrChanSwitch) ErrChan() chan<- error {
	return s.incomingChan
}

// Divert causes all incoming error messages to be sent to the given temporary
// channel from now on instead of the main error channel.
func (s *ErrChanSwitch) Divert(tempChan chan<- error) {
	s.Lock()
	defer s.Unlock()

	s.tempChan = tempChan
	s.diverted = true
}

// Restore causes all incoming error messages to be sent to the main channel
// again from now on.
func (s *ErrChanSwitch) Restore() {
	s.Lock()
	defer s.Unlock()

	s.tempChan = nil
	s.diverted = false
}

// Start spins up the internal goroutine that processes incoming messages.
func (s *ErrChanSwitch) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run()
	}()
}

// Stop shuts down the goroutine that processes incoming messages.
func (s *ErrChanSwitch) Stop() {
	close(s.quit)
	s.wg.Wait()
}

// run is the main event loop where we receive errors on the incoming channel
// and send them to the correct outgoing channel in a concurrency safe way.
func (s *ErrChanSwitch) run() {
	for {
		select {
		case msg := <-s.incomingChan:
			// Make sure that while we are processing a message the
			// channels can't be switched.
			s.Lock()
			if s.diverted {
				select {
				case s.tempChan <- msg:
				case <-s.quit:
				}
			} else {
				select {
				case s.mainChan <- msg:
				case <-s.quit:
				}
			}
			s.Unlock()

		case <-s.quit:
			return
		}
	}
}
