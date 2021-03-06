// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mindreader

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dfuse-io/bstream"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type ArchiverSelector struct {
	oneblockArchiver Archiver
	mergeArchiver    Archiver

	blockReaderFactory bstream.BlockReaderFactory

	firstBlockPassed    bool
	firstBoundaryPassed bool
	currentlyMerging    bool

	batchMode              bool // forces merging blocks without tracker or LIB checking
	tracker                *bstream.Tracker
	mergeThresholdBlockAge time.Duration
	lastSeenLIB            *atomic.Uint64

	workDir string
	logger  *zap.Logger
}

func NewArchiverSelector(
	oneblockArchiver Archiver,
	mergeArchiver Archiver,
	blockReaderFactory bstream.BlockReaderFactory,
	batchMode bool,
	tracker *bstream.Tracker,
	mergeThresholdBlockAge time.Duration,
	workDir string,
	logger *zap.Logger,
) *ArchiverSelector {
	s := &ArchiverSelector{
		oneblockArchiver:       oneblockArchiver,
		mergeArchiver:          mergeArchiver,
		blockReaderFactory:     blockReaderFactory,
		batchMode:              batchMode,
		tracker:                tracker,
		mergeThresholdBlockAge: mergeThresholdBlockAge,
		lastSeenLIB:            atomic.NewUint64(0),
		workDir:                workDir,
		logger:                 logger,
	}
	if !batchMode {
		s.launchLastLIBUpdater()
	}
	return s
}

func (s *ArchiverSelector) updateLastLIB() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ref, err := s.tracker.Get(ctx, bstream.NetworkLIBTarget)
	if err != nil {
		return err
	}
	s.lastSeenLIB.Store(ref.Num())
	return nil
}

func (s *ArchiverSelector) launchLastLIBUpdater() {
	var fetchedOnce bool
	var warned bool
	var err error
	sleepTime := 200 * time.Millisecond

	err = s.updateLastLIB()
	if err == nil { // don't warn on first error, maybe blockmeta is booting with us
		fetchedOnce = true
		sleepTime = 30 * time.Second
	}

	go func() {
		for {
			time.Sleep(sleepTime)

			if s.firstBoundaryPassed {
				if !s.currentlyMerging { // we will never 'go back' to merging
					if !fetchedOnce {
						s.logger.Warn("could not get LIB from blockmeta after a few attempts. Not merging blocks", zap.Error(err))
					}
					return
				}
				sleepTime = 30 * time.Second

				if !fetchedOnce && !warned {
					s.logger.Warn("cannot get LIB from blockmeta, merging blocks based on their blocktime only (will retry)", zap.Duration("merge_threshold_block_age", s.mergeThresholdBlockAge))
					warned = true
				}
			}

			err = s.updateLastLIB()
			if err == nil {
				if !fetchedOnce {
					fetchedOnce = true
					sleepTime = 30 * time.Second
					if warned {
						s.logger.Warn("success connecting to blockmeta after previous failures")
					}
				}
			}
		}
	}()
}

func (s *ArchiverSelector) shouldSendToMergeArchiver(block *bstream.Block) bool {
	if s.batchMode {
		s.logger.Info("merging next blocks directly because we are in batch mode")
		return true
	}

	blockNum := block.Num()
	lastSeenLIB := s.lastSeenLIB.Load()

	blockAge := time.Since(block.Time())
	if blockAge > s.mergeThresholdBlockAge {
		s.logger.Info("merging next blocks directly because they are older than threshold", zap.Uint64("block_num", blockNum), zap.Duration("block_age", blockAge))
		return true
	}

	if blockNum+100 <= lastSeenLIB {
		s.logger.Info("merging next blocks directly because they are older than LIB", zap.Uint64("block_num", blockNum), zap.Uint64("lib", lastSeenLIB))
		return true
	}

	s.logger.Info("producing one-block files...", zap.Uint64("block_num", blockNum))
	return false
}

func (s *ArchiverSelector) loadLastPartial(next uint64) []*bstream.Block {
	matches, err := filepath.Glob(filepath.Join(s.workDir, "archiver_*.partial"))
	if err != nil {
		s.logger.Warn("trying to find glob for partial archive", zap.Error(err))
		return nil
	}
	for _, match := range matches {
		saved := filepath.Base(match)
		if len(saved) != 27 {
			s.logger.Error("trying to restore partial archive but got invalid filename", zap.String("saved", saved), zap.Int("length", len(saved)))
			continue
		}
		savedNum, err := strconv.ParseUint(saved[9:19], 10, 64)
		if err != nil {
			s.logger.Error("trying to restore partial archive but got invalid number from filename", zap.String("saved", saved[9:19]), zap.Error(err))
			continue
		}
		if savedNum != next {
			s.logger.Info("last partial block file does not match saved, deleting file", zap.Uint64("next", next), zap.Uint64("saved_partial", savedNum))
			os.Remove(match)
			continue
		}

		f, err := os.Open(match)
		if err != nil {
			s.logger.Error("trying to restore partial archive but got cannot open file. deleting it", zap.String("filename", match), zap.Error(err))
			os.Remove(match)
			continue
		}

		blockReader, err := s.blockReaderFactory.New(f)
		if err != nil {
			s.logger.Error("trying to generate blockreader with file on restore", zap.Error(err))
			f.Close()
			return nil
		}

		var blocks []*bstream.Block
		for {
			blk, err := blockReader.Read()
			if blk != nil {
				blocks = append(blocks, blk)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				s.logger.Error("trying to read block on partial archive restore caused an error. deleting file", zap.Error(err))
				os.Remove(match)
				f.Close()
				return nil
			}
		}
		f.Close()
		os.Remove(match)
		return blocks
	}

	return nil
}

func (s *ArchiverSelector) chooseArchiver(merging bool) Archiver {
	if merging {
		return s.mergeArchiver
	}
	return s.oneblockArchiver
}

func (s *ArchiverSelector) StoreBlock(block *bstream.Block) error {
	if s.firstBoundaryPassed && !s.currentlyMerging {
		return s.oneblockArchiver.StoreBlock(block) // once we passed a boundary creating oneblocks, we never go back to merging, too risky
	}

	blockNum := block.Num()
	isBoundaryBlock := (blockNum%100 == 0 || blockNum == bstream.GetProtocolFirstStreamableBlock)

	if !s.firstBlockPassed {
		s.firstBlockPassed = true
		s.currentlyMerging = s.shouldSendToMergeArchiver(block)

		if isBoundaryBlock {
			s.firstBoundaryPassed = true
			return s.chooseArchiver(s.currentlyMerging).StoreBlock(block)
		}

		previousBlocks := s.loadLastPartial(blockNum)
		if previousBlocks == nil {
			s.currentlyMerging = false // we cannot merge blocks without the partial load
		}

		for _, blk := range previousBlocks {
			if err := s.chooseArchiver(s.currentlyMerging).StoreBlock(blk); err != nil {
				return err
			}
		}
		return s.chooseArchiver(s.currentlyMerging).StoreBlock(block)
	}

	if !isBoundaryBlock {
		return s.chooseArchiver(s.currentlyMerging).StoreBlock(block) // don't change your operation mode between boundaries
	}

	s.firstBoundaryPassed = true
	// WE ARE AT A BOUNDARY! YAY

	previouslyMerging := s.currentlyMerging
	s.currentlyMerging = s.shouldSendToMergeArchiver(block)

	// when we are producing one-block files at startup, we may get to the first boundary and then decide to merge
	//  ex:  blocks 195, 196, 197, 198, 199 [200 merge ...]
	// the `merger` requires the upper bound block (200) to be able to merge
	// to accomodate the behavior of the merger, we will send that block '200' to both archivers
	sendToBoth := !previouslyMerging && s.currentlyMerging
	if sendToBoth {
		if err := s.oneblockArchiver.StoreBlock(block); err != nil {
			return err
		}
		return s.mergeArchiver.StoreBlock(block)
	}

	return s.chooseArchiver(s.currentlyMerging).StoreBlock(block)

}

func (s *ArchiverSelector) Start() {
	s.logger.Info("Starting OneBlock uploads")
	go s.oneblockArchiver.Start()
	s.logger.Info("Starting MergedBlocks uploads")
	go s.mergeArchiver.Start()
}

// Terminate assumes that no more 'StoreBlock' command is coming
func (s *ArchiverSelector) Terminate() <-chan interface{} {
	ch := make(chan interface{})
	go func() {
		<-s.mergeArchiver.Terminate()
		<-s.oneblockArchiver.Terminate()
		close(ch)
	}()
	return ch
}

func (s *ArchiverSelector) Init() error {
	if err := s.oneblockArchiver.Init(); err != nil {
		return err
	}
	return s.mergeArchiver.Init()
}
