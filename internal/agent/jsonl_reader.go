package agent

import (
	"bufio"
	"io"
	"sync"
)

// A transcript is checked after every agy step and on each Codex watcher poll,
// often while its writer has nothing new to report. Reuse the existing buffer
// size across those short reads instead of allocating 256 KiB per file/poll.
// The pool is shared by all CLI runs, is safe for concurrent readers, and may
// release idle buffers at GC. No buffer or open file is retained per subagent.
var transcriptReaders = sync.Pool{New: func() any {
	return bufio.NewReaderSize(nil, 256*1024)
}}

func takeTranscriptReader(source io.Reader) *bufio.Reader {
	r := transcriptReaders.Get().(*bufio.Reader)
	r.Reset(source)
	return r
}

func putTranscriptReader(r *bufio.Reader) {
	// Discard buffered partial lines: offsets deliberately only advance for
	// accepted lines, so the next poll must read them from disk again. Also
	// detach the closed source instead of retaining it in the shared pool.
	r.Reset(nil)
	transcriptReaders.Put(r)
}
