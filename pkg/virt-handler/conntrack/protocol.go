/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package conntrack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire format: [version:byte][data_len:uint32][data:bytes]
type SyncMessage struct {
	Version byte
	Data    []byte
}

func (m *SyncMessage) Encode() []byte {
	dataLen := len(m.Data)
	buf := make([]byte, 1+4+dataLen)

	buf[0] = m.Version

	binary.BigEndian.PutUint32(buf[1:5], uint32(dataLen))
	copy(buf[5:], m.Data)

	return buf
}

// readChunkSize bounds a single allocation while reading the message payload.
const readChunkSize = 1024 * 1024 // 1 MiB

// maxMessageSize caps the payload of a single sync message. 512 MiB holds about
// 1.2M conntrack entries, well above what a VM can accumulate, while a message
// that large could not reach the target within SyncTimeout anyway.
const maxMessageSize = 512 * 1024 * 1024 // 512 MiB

func DecodeSyncMessage(r io.Reader) (*SyncMessage, error) {
	var version byte
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return nil, fmt.Errorf("failed to read version: %w", err)
	}

	// Do not trust incoming data. Read dataLen, reject it if it is above the
	// limit, and otherwise use it only to check buffer length after reading.
	// Use incoming dataLen to allocate buffer may lead to amplification
	// attacks (5 bytes -> 4GiB allocation).
	var dataLen uint32
	if err := binary.Read(r, binary.BigEndian, &dataLen); err != nil {
		return nil, fmt.Errorf("failed to read data length: %w", err)
	}

	if dataLen > maxMessageSize {
		return nil, fmt.Errorf("declared data length %d exceeds the limit of %d bytes", dataLen, maxMessageSize)
	}

	data, err := readPayload(r, dataLen)
	if err != nil {
		return nil, fmt.Errorf("failed to read data: %w", err)
	}

	if uint64(len(data)) != uint64(dataLen) {
		return nil, fmt.Errorf("data length mismatch: declared %d bytes, got %d", dataLen, len(data))
	}

	return &SyncMessage{
		Version: version,
		Data:    data,
	}, nil
}

// readPayload reads dataLen bytes in readChunkSize steps. The buffer grows only
// as bytes actually arrive, so a bogus length costs nothing until it is backed
// by real data. Reading never goes past dataLen: bytes that follow the message
// stay in the stream, and no EOF is required to finish. A stream that ends early
// yields fewer bytes and the caller reports the mismatch.
func readPayload(r io.Reader, dataLen uint32) ([]byte, error) {
	chunkSize := uint64(dataLen)
	if chunkSize > readChunkSize {
		chunkSize = readChunkSize
	}
	if chunkSize == 0 {
		return nil, nil
	}
	chunk := make([]byte, chunkSize)

	var data []byte
	for uint64(len(data)) < uint64(dataLen) {
		limit := uint64(dataLen) - uint64(len(data))
		if limit > uint64(len(chunk)) {
			limit = uint64(len(chunk))
		}

		n, err := io.ReadFull(r, chunk[:limit])
		data = append(data, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return data, nil
			}
			return nil, err
		}
	}

	return data, nil
}
