import unittest
from petals_backend.network import private_bootstrap

PEER = "12D3KooWQ7ZQbNgKgpYM35Rm9NWgKzTmxmtMRpaTYzkmE4xMqGuq"


class BootstrapPolicy(unittest.TestCase):
    def test_private_literal_addresses(self):
        for family, ip in (("ip4", "10.1.2.3"), ("ip4", "100.64.1.2"), ("ip4", "127.0.0.1"), ("ip6", "fd00::1")):
            value = f"/{family}/{ip}/tcp/31330/p2p/{PEER}"
            self.assertEqual(private_bootstrap(value), value)

    def test_no_public_discovery_dns_or_metadata(self):
        for address in (f"/ip4/8.8.8.8/tcp/31330/p2p/{PEER}", f"/dns4/petals.example/tcp/31330/p2p/{PEER}",
                        f"/ip4/169.254.169.254/tcp/80/p2p/{PEER}", f"/ip4/0.0.0.0/tcp/31330/p2p/{PEER}",
                        f"/ip4/10.0.0.1/tcp/0/p2p/{PEER}", f"/ip6/2001:db8::1/tcp/1234/p2p/{PEER}",
                        f"/ip4/10.0.0.1/udp/1234/p2p/{PEER}", "/ip4/10.0.0.1/tcp/1234/p2p/bad"):
            with self.subTest(address=address), self.assertRaises(ValueError):
                private_bootstrap(address)


if __name__ == "__main__": unittest.main()
