package multi

import (
	"fmt"

	"github.com/pensando/box/builder"
	"github.com/pensando/box/logger"
	"github.com/pensando/box/types"
)

// Builder is the entrypoint to the multi-build system. It contains several
// builders.
type Builder struct {
	builders []*builder.Builder
}

// NewBuilder contypes a *Builder.
func NewBuilder(builders []*builder.Builder) *Builder {
	return &Builder{builders: builders}
}

// Build builds all the builders in parallel.
func (b *Builder) Build() {
	for _, br := range b.builders {
		go br.Run()
	}
}

// Wait waits for all builds to complete.
func (b *Builder) Wait() error {
	log := logger.New("multi", false)

	resChan := make(chan types.BuildResult, len(b.builders))

	for _, br := range b.builders {
		go func(br *builder.Builder) {
			resChan <- br.Wait()
		}(br)
	}

	var errored bool

	for i := 0; i < len(b.builders); i++ {
		res := <-resChan
		if res.Err != nil {
			errored = true
			log.Error(fmt.Sprintf("%s: error occurred during plan execution: %v", res.FileName, res.Err))
		}
	}

	if errored {
		return fmt.Errorf("some builds contained errors")
	}

	return nil
}
