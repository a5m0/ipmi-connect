// Package supermicro is a client for the ATEN firmware on Supermicro X7SB3 BMCs (WPCM450).
//
// The KVM is RFB 3.8 on port 5900 with ATEN's own security type 16. Power control
// goes through lighttpd CGI endpoints authenticated by a SID cookie.
//
// ATEN firmware on ASPEED BMCs sends ASPEED-compressed video (encoding 0x57) rather than Hermon;
// both are decoded.
package supermicro
