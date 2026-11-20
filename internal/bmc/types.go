package bmc

// TargetType identifies the BMC vendor
type TargetType string

const (
	TargetAuto       TargetType = "auto"
	TargetDell       TargetType = "dell"
	TargetSupermicro TargetType = "supermicro"
	TargetMegaRAC    TargetType = "megarac" // AMI MegaRAC SP (ASUS ASMB8-iKVM)
)

// SessionState holds status and telemetry of an active BMC session
type SessionState struct {
	Host          string     `json:"host"`
	TargetType    TargetType `json:"target_type"`
	Model         string     `json:"model"`
	Firmware      string     `json:"firmware"`
	Connected     bool       `json:"connected"`
	Authenticated bool       `json:"authenticated"`
	PowerState    string     `json:"power_state"`
	ScreenWidth   int        `json:"screen_width"`
	ScreenHeight  int        `json:"screen_height"`
	FPS           int        `json:"fps"`
	// PingMS is the round trip to the BMC in milliseconds; 0 while unknown.
	PingMS       float64 `json:"ping_ms,omitempty"`
	StatusText   string  `json:"status_text"`
	VirtualMedia string  `json:"virtual_media"`
	// Channels describes how each channel to the BMC is protected, for the security indicator.
	Channels []ChannelSecurity `json:"channels"`
	// SecurityLevel summarises Channels: SecurityOK, SecurityLegacy or SecurityInsecure.
	SecurityLevel string `json:"security_level,omitempty"`
}

// Encryption of a channel.
const (
	EncTLS       = "tls"        // TLS 1.2 or later with a modern suite
	EncLegacyTLS = "legacy-tls" // TLS 1.0/1.1 or a CBC/3DES suite, authorised by the user
	EncWeak      = "weak"       // obsolete or publicly keyed cipher: treat as unencrypted
	EncNone      = "none"       // clear text
)

// Certificate authentication of a channel.
const (
	AuthCA     = "ca"     // CA-verified certificate
	AuthPinned = "pinned" // fingerprint pinned by the user
	AuthNone   = "none"   // no certificate: the peer is not authenticated
)

// Overall connection security.
const (
	SecurityOK       = "secure"
	SecurityLegacy   = "legacy"
	SecurityInsecure = "insecure"
)

// ChannelSecurity is how one channel to the BMC is protected. Encryption (are the bytes
// protected) and authentication (is the peer the BMC) are reported separately.
type ChannelSecurity struct {
	Channel     string `json:"channel"`
	Encryption  string `json:"encryption"`
	Protocol    string `json:"protocol,omitempty"`
	Cipher      string `json:"cipher,omitempty"`
	Auth        string `json:"auth"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Note        string `json:"note,omitempty"`
}

// SecurityLevelOf summarises channels: insecure if any channel is unencrypted or weak, legacy if
// any uses obsolete TLS, otherwise secure.
func SecurityLevelOf(channels []ChannelSecurity) string {
	level := SecurityOK
	for _, c := range channels {
		switch c.Encryption {
		case EncNone, EncWeak:
			return SecurityInsecure
		case EncLegacyTLS:
			level = SecurityLegacy
		}
	}
	return level
}

// PowerAction represents a chassis power command
type PowerAction string

const (
	PowerOn      PowerAction = "power_on"
	PowerOff     PowerAction = "power_off"
	PowerReset   PowerAction = "power_reset"
	PowerCycle   PowerAction = "power_cycle"
	PowerSoftOff PowerAction = "power_soft_off"
)
