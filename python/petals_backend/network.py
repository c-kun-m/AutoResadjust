"""Explicit bootstrap address policy, independent of Petals imports."""
import ipaddress
import re

_ALLOWED = tuple(map(ipaddress.ip_network, (
    "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
    "100.64.0.0/10", "fc00::/7", "::1/128",
)))
_PEER = re.compile(r"[1-9A-HJ-NP-Za-km-z]{32,128}\Z")


def private_bootstrap(address):
    parts = address.split("/")
    if len(parts) != 7 or parts[0] or parts[1] not in ("ip4", "ip6") or parts[3] != "tcp" or parts[5] != "p2p":
        raise ValueError("bootstrap must be /ip4|ip6/private-IP/tcp/port/p2p/peer-ID")
    ip = ipaddress.ip_address(parts[2])
    if (ip.version == 4) != (parts[1] == "ip4") or not any(ip in network for network in _ALLOWED):
        raise ValueError("bootstrap must use a literal LAN, VPN or loopback address")
    if not parts[4].isascii() or not parts[4].isdigit() or not 1 <= int(parts[4]) <= 65535 or not _PEER.fullmatch(parts[6]):
        raise ValueError("invalid bootstrap TCP port or peer ID")
    return address


def private_dht(peers, **kwargs):
    from hivemind import DHT
    if not peers or len(peers) > 8:
        raise ValueError("1..8 explicit private bootstrap peers are required")
    peers = [private_bootstrap(peer) for peer in peers]
    # A private VPN/firewall is still required: bootstrap validation is not a
    # firewall for addresses announced by other participants inside the swarm.
    return DHT(initial_peers=peers, use_relay=False, use_auto_relay=False, **kwargs)
