// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hostsensors

import (
	"time"

	"github.com/Microsoft/go-winio"
)

// Read connects to the helper's pipe, reads one Report and disconnects.
// timeout bounds the connect and the read separately; an absent helper
// fails fast (the pipe does not exist) rather than waiting for one to appear.
func Read(timeout time.Duration) (Report, error) {
	conn, err := winio.DialPipe(PipeName, &timeout)
	if err != nil {
		return Report{}, err
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return Report{}, err
	}
	return Decode(conn)
}
