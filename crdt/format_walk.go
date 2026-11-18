package crdt

// WalkFormatting visits non-deleted formatting markers in document order, including
// boundaries without visible text. A nil value ends a formatting span.
// It stops on the first callback error and returns that error unchanged.
//
// Call outside transactions. The document read lock is held during callbacks,
// which must not mutate values or call document APIs.
func (txt *YText) WalkFormatting(visit func(key string, value any) error) error {
	if doc := txt.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	for item := txt.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		if format, ok := item.Content.(*ContentFormat); ok {
			if err := visit(format.Key, format.Val); err != nil {
				return err
			}
		}
	}
	return nil
}
