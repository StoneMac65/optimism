package reassemble

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"sort"

	"github.com/ethereum-optimism/optimism/op-node/cmd/batch_decoder/fetch"
	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum/go-ethereum/common"
)

type ChannelWithMeta struct {
	ID            derive.ChannelID    `json:"id"`
	IsReady       bool                `json:"is_ready"`
	InvalidFrames bool                `json:"invalid_frames"`
	Frames        []FrameWithMetadata `json:"frames"`
	SkippedFrames []FrameWithMetadata `json:"skipped_frames"`
}

type FrameWithMetadata struct {
	TxHash         common.Hash  `json:"transaction_hash"`
	InclusionBlock uint64       `json:"inclusion_block"`
	Frame          derive.Frame `json:"frame"`
}

type Config struct {
	BatchInbox   common.Address
	InDirectory  string
	OutDirectory string
}

// Channels loads all transactions from the given input directory that are submitted to the
// specified batch inbox and then re-assembles all channels & writes the re-assembled channels
// to the out directory.
func Channels(config Config) {
	if err := os.MkdirAll(config.OutDirectory, 0750); err != nil {
		log.Fatal(err)
	}
	txns := loadTransactions(config.InDirectory, config.BatchInbox)
	// Sort first by block number then by transaction index inside the block number range.
	// This is to match the order they are processed in derivation.
	sort.Slice(txns, func(i, j int) bool {
		if txns[i].BlockNumber == txns[j].BlockNumber {
			return txns[i].TxIndex < txns[j].TxIndex
		} else {
			return txns[i].BlockNumber < txns[j].BlockNumber
		}

	})
	frames := transactionsToFrames(txns)
	framesByChannel := make(map[derive.ChannelID][]FrameWithMetadata)
	for _, frame := range frames {
		framesByChannel[frame.Frame.ID] = append(framesByChannel[frame.Frame.ID], frame)
	}
	for id, frames := range framesByChannel {
		ch := processFrames(id, frames)
		filename := path.Join(config.OutDirectory, fmt.Sprintf("%s.json", id.String()))
		file, err := os.Create(filename)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()
		enc := json.NewEncoder(file)
		if err := enc.Encode(ch); err != nil {
			log.Fatal(err)
		}
	}
}

func processFrames(id derive.ChannelID, frames []FrameWithMetadata) ChannelWithMeta {
	// This code is roughly copied from rollup/derive/channel.go
	// We will use that file to reconstruct the batches, but need to implement this manually
	// to figure out which frames got pruned.
	var skippedFrames []FrameWithMetadata
	framesByNumber := make(map[uint16]FrameWithMetadata)
	closed := false
	var endFrameNumber, highestFrameNumber uint16
	for _, frame := range frames {
		if frame.Frame.IsLast && closed {
			fmt.Println("Trying to close channel twice")
			skippedFrames = append(skippedFrames, frame)
			continue
		}
		if _, ok := framesByNumber[frame.Frame.FrameNumber]; ok {
			fmt.Println("Duplicate frame")
			skippedFrames = append(skippedFrames, frame)
			continue
		}
		if closed && frame.Frame.FrameNumber >= endFrameNumber {
			fmt.Println("Frame number past the end of the channel")
			skippedFrames = append(skippedFrames, frame)
			continue
		}
		framesByNumber[frame.Frame.FrameNumber] = frame
		if frame.Frame.IsLast {
			endFrameNumber = frame.Frame.FrameNumber
			closed = true
		}

		if frame.Frame.IsLast && endFrameNumber < highestFrameNumber {
			// Do a linear scan over saved inputs instead of ranging over ID numbers
			for id, prunedFrame := range framesByNumber {
				if id >= endFrameNumber {
					skippedFrames = append(skippedFrames, prunedFrame)
				}
			}
			highestFrameNumber = endFrameNumber
		}

		if frame.Frame.FrameNumber > highestFrameNumber {
			highestFrameNumber = frame.Frame.FrameNumber
		}
	}
	ready := chReady(framesByNumber, closed, endFrameNumber)

	if !ready {
		fmt.Printf("Found channel that was not closed: %v\n", id.String())
	}
	return ChannelWithMeta{
		ID:            id,
		Frames:        frames,
		SkippedFrames: skippedFrames,
		IsReady:       ready,
		InvalidFrames: len(skippedFrames) != 0,
	}
}

func chReady(inputs map[uint16]FrameWithMetadata, closed bool, endFrameNumber uint16) bool {
	if !closed {
		return false
	}
	if len(inputs) != int(endFrameNumber)+1 {
		return false
	}
	// Check for contiguous frames
	for i := uint16(0); i <= endFrameNumber; i++ {
		_, ok := inputs[i]
		if !ok {
			return false
		}
	}
	return true
}

func transactionsToFrames(txns []fetch.TransactionWithMeta) []FrameWithMetadata {
	var out []FrameWithMetadata
	for _, tx := range txns {
		for _, frame := range tx.Frames {
			fm := FrameWithMetadata{
				TxHash:         tx.Tx.Hash(),
				InclusionBlock: tx.BlockNumber,
				Frame:          frame,
			}
			out = append(out, fm)
		}
	}
	return out
}

func loadTransactions(dir string, inbox common.Address) []fetch.TransactionWithMeta {
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}
	var out []fetch.TransactionWithMeta
	for _, file := range files {
		f := path.Join(dir, file.Name())
		txm := loadTransactionsFile(f)
		if txm.InboxAddr == inbox && txm.ValidSender {
			out = append(out, txm)
		}
	}
	return out
}

func loadTransactionsFile(file string) fetch.TransactionWithMeta {
	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var txm fetch.TransactionWithMeta
	if err := dec.Decode(&txm); err != nil {
		log.Fatalf("Failed to decode %v. Err: %v\n", file, err)
	}
	return txm
}
