// SPDX-License-Identifier: GPL-3.0-only

// Package rawsock is the Sampled Values subscriber's receive socket: an
// AF_PACKET socket like the library's ethernet.Open, but with a large
// kernel receive buffer and one reused read buffer. At 4000 frames/s the
// default buffer (rmem_default, about 200 KiB) holds well under 100 ms
// of frames, so a short scheduling stall of the reader loses samples.
package rawsock

import "time"

// RcvBuf is the receive buffer requested; the kernel clamps it to
// net.core.rmem_max unless the process has CAP_NET_ADMIN.
const RcvBuf = 32 << 20

// pollInterval bounds how long Read blocks, so Close unblocks it.
const pollInterval = 200 * time.Millisecond
