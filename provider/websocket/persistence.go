package websocket

import "log"

// startPersistenceWorker spawns the goroutine that drains r.persistCh and
// forwards each update to the PersistenceAdapter. It must be called with
// r.persistCh, r.persistStop, and r.persistDone already initialised.
//
// The worker exits when either r.persistStop or s.shutdownCh is closed,
// draining any buffered updates before returning so that no committed
// transaction is silently lost.
func (s *Server) startPersistenceWorker(r *room, name string) {
	go func() {
		defer close(r.persistDone)
		store := func(update []byte) {
			defer func() {
				if rv := recover(); rv != nil {
					log.Printf("ygo/websocket: StoreUpdate panic for room %q: %v", name, rv)
				}
			}()
			if err := s.persistence.StoreUpdate(name, update); err != nil {
				log.Printf("ygo/websocket: StoreUpdate for room %q: %v", name, err)
			}
		}
		for {
			select {
			case update := <-r.persistCh:
				store(update)
			case <-r.persistStop:
				// Drain buffered updates before exiting.
				for {
					select {
					case update := <-r.persistCh:
						store(update)
					default:
						return
					}
				}
			case <-s.shutdownCh:
				// Server is shutting down; drain any remaining buffered updates.
				for {
					select {
					case update := <-r.persistCh:
						store(update)
					default:
						return
					}
				}
			}
		}
	}()
}
