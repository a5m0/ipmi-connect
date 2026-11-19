// Package supermicro is a client for the ATEN firmware on Supermicro X7SB3 BMCs (WPCM450).
//
// The KVM is RFB 3.8 on port 5900 with ATEN's own security type 16. Power control
// goes through lighttpd CGI endpoints authenticated by a SID cookie.
//
// Some boards pair the same ATEN iKVM with AMI MegaRAC's classic web interface (the ASUS
// ASMB8-iKVM, an AST2400): there login, tickets, power and logout go through /rpc/*.asp pages
// (MegaRACWeb), and the video is ASPEED-compressed (encoding 0x57) rather than Hermon.
package supermicro
