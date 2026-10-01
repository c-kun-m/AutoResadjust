"""Llama client with an explicitly owned private DHT.

Petals' public CausalLM constructor does not expose its underlying model's dht
argument. Keep the small constructor adapter here instead of replacing global
DHT factories or permitting the library's default discovery configuration.
"""
from petals.client.lm_head import LMHead
from petals.models.llama.model import DistributedLlamaForCausalLM, DistributedLlamaModel
from transformers.models.llama import LlamaPreTrainedModel


class PrivateLlama(DistributedLlamaForCausalLM):
    def __init__(self, config, *, dht):
        if not config.initial_peers or not config.allowed_servers:
            raise ValueError("private bootstrap peers and assigned server IDs are required")
        if config.max_retries != 1:
            raise ValueError("private inference permits one attempt, without automatic replay")
        LlamaPreTrainedModel.__init__(self, config)
        self.model = DistributedLlamaModel(config, dht=dht)
        self.pretraining_tp = config.pretraining_tp
        self.vocab_size = config.vocab_size
        self.lm_head = LMHead(config)
        self.post_init()

    def close(self):
        # DHT lifetime belongs to the caller; stop the route updater first.
        self.transformer.h.sequence_manager.shutdown()
