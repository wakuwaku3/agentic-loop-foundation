package provider

import "errors"

type Handoff struct {
	Version      string     `json:"version"`
	FromProvider string     `json:"from_provider"`
	ToProvider   string     `json:"to_provider"`
	Packet       WorkPacket `json:"packet"`
	Failure      Failure    `json:"failure,omitempty"`
	Usage        Usage      `json:"usage,omitempty"`
	OutputDigest string     `json:"output_digest,omitempty"`
}

func PrepareHandoff(from, to string, packet WorkPacket, result Result) (Handoff, error) {
	if from == "" || to == "" || from == to {
		return Handoff{}, ErrInvalidRequest
	}
	if err := packet.Validate(); err != nil {
		return Handoff{}, err
	}
	if result.Provider != "" && result.Provider != from {
		return Handoff{}, errors.New("handoff provider mismatch")
	}
	h := Handoff{Version: ContractVersion, FromProvider: from, ToProvider: to, Packet: packet, Usage: result.Usage, OutputDigest: result.OutputDigest}
	if result.Failure != nil {
		h.Failure = *result.Failure
	}
	return h, nil
}
