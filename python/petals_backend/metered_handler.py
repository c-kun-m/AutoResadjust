"""Petals inference boundary instrumentation, shared across handler processes.

DHT discovery, TCP framing and encryption bytes are not inference payload.
The runtime disables server-to-server speculative push in its client profile;
received pushes are still measured if a trusted client explicitly sends one.
"""
import time
from typing import AsyncIterator

from hivemind import P2PContext
from hivemind.proto import runtime_pb2
from petals.server.handler import TransformerConnectionHandler


class MeteredHandler(TransformerConnectionHandler):
    @classmethod
    def _get_handle_name(cls, namespace, method_name):
        # Preserve the upstream wire protocol while subclassing for telemetry.
        return TransformerConnectionHandler._get_handle_name(namespace, method_name)

    def __init__(self, *args, counters, **kwargs):
        self.counters = counters
        super().__init__(*args, **kwargs)

    async def rpc_inference(self, requests: AsyncIterator[runtime_pb2.ExpertRequest],
                            context: P2PContext) -> AsyncIterator[runtime_pb2.ExpertResponse]:
        began = time.monotonic()
        self.counters.add(sessions_total=1, active_sessions=1)

        async def counted():
            async for request in requests:
                self.counters.add(rx_bytes_total=request.ByteSize())
                yield request

        try:
            async for response in super().rpc_inference(counted(), context):
                self.counters.add(tx_bytes_total=response.ByteSize(), steps_total=1)
                yield response
        except BaseException:
            self.counters.add(errors_total=1)
            raise
        finally:
            self.counters.add(active_sessions=-1, duration_seconds_total=time.monotonic() - began)

    async def rpc_push(self, request: runtime_pb2.ExpertRequest,
                       context: P2PContext) -> runtime_pb2.ExpertResponse:
        self.counters.add(rx_bytes_total=request.ByteSize())
        response = await super().rpc_push(request, context)
        self.counters.add(tx_bytes_total=response.ByteSize())
        return response

    async def rpc_forward(self, request: runtime_pb2.ExpertRequest,
                          context: P2PContext) -> runtime_pb2.ExpertResponse:
        raise ValueError("this worker only serves inference sessions")

    async def rpc_backward(self, request: runtime_pb2.ExpertRequest,
                           context: P2PContext) -> runtime_pb2.ExpertResponse:
        raise ValueError("training is disabled")

    async def rpc_forward_stream(self, requests: AsyncIterator[runtime_pb2.ExpertRequest],
                                 context: P2PContext) -> AsyncIterator[runtime_pb2.ExpertResponse]:
        raise ValueError("this worker only serves inference sessions")
        yield  # Preserve the async-generator RPC signature.

    async def rpc_backward_stream(self, requests: AsyncIterator[runtime_pb2.ExpertRequest],
                                  context: P2PContext) -> AsyncIterator[runtime_pb2.ExpertResponse]:
        raise ValueError("training is disabled")
        yield
