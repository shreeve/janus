package main

import "errors"

// ownSockets: the Windows reader (GetExtendedTcpTable) lands with exposure
// modes there; until then the edge cannot verify its bind and says so.
func ownSockets() ([]boundSocket, error) {
	return nil, errors.New("exposure modes are not supported on windows yet; use the portable high-port mode")
}
