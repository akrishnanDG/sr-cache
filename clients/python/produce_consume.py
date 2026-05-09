"""
Python Avro producer + consumer using Confluent Kafka client and the
confluent_kafka.schema_registry.avro serializer/deserializer.

The Schema Registry endpoint is the LOCAL CACHING PROXY — schema registration,
schema-id lookups, and subject queries all flow through the proxy.
"""
import os
import sys
import time
import uuid
from pathlib import Path

from confluent_kafka import Producer, Consumer, KafkaException
from confluent_kafka.serialization import (
    SerializationContext,
    MessageField,
    StringSerializer,
    StringDeserializer,
)
from confluent_kafka.schema_registry import SchemaRegistryClient
from confluent_kafka.schema_registry.avro import AvroSerializer, AvroDeserializer


def load_env(path):
    for line in Path(path).read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        k, _, v = line.partition("=")
        os.environ.setdefault(k.strip(), v.strip())


CLIENTS_DIR = Path(__file__).resolve().parent.parent
load_env(CLIENTS_DIR / ".env")

SCHEMA_STR = (CLIENTS_DIR / "schema.avsc").read_text()
TOPIC = os.environ["TOPIC"]
CLIENT_TAG = "python"


def kafka_conf():
    return {
        "bootstrap.servers": os.environ["BOOTSTRAP_SERVERS"],
        "security.protocol": "SASL_SSL",
        "sasl.mechanisms": "PLAIN",
        "sasl.username": os.environ["KAFKA_API_KEY"],
        "sasl.password": os.environ["KAFKA_API_SECRET"],
    }


def sr_client():
    # NOTE: pointed at the local proxy. No basic auth here — the proxy injects
    # upstream credentials. This is exactly the user-facing intent of the
    # proxy: clients are unaware of upstream auth.
    return SchemaRegistryClient({"url": os.environ["SR_URL"]})


def to_dict(obj, ctx):
    return obj


def from_dict(obj, ctx):
    return obj


def produce(n=3):
    sr = sr_client()
    avro_ser = AvroSerializer(sr, SCHEMA_STR, to_dict=to_dict)
    key_ser = StringSerializer("utf_8")

    p = Producer(kafka_conf())
    delivered = []

    def cb(err, msg):
        if err:
            print(f"  delivery error: {err}", file=sys.stderr)
            return
        delivered.append((msg.partition(), msg.offset()))

    run_id = uuid.uuid4().hex[:8]
    for i in range(n):
        rec = {"id": i, "msg": f"hello from python ({run_id})", "client": CLIENT_TAG}
        p.produce(
            topic=TOPIC,
            key=key_ser(f"{CLIENT_TAG}-{run_id}-{i}"),
            value=avro_ser(rec, SerializationContext(TOPIC, MessageField.VALUE)),
            on_delivery=cb,
        )
    p.flush(15)
    if len(delivered) != n:
        print(f"only {len(delivered)}/{n} messages delivered", file=sys.stderr)
        sys.exit(2)
    print(f"[python] produced {n} messages: partitions/offsets = {delivered}")
    return run_id


def consume(run_id, expected=3, timeout_s=30):
    sr = sr_client()
    avro_de = AvroDeserializer(sr, SCHEMA_STR, from_dict=from_dict)
    key_de = StringDeserializer("utf_8")

    conf = kafka_conf()
    conf.update({
        "group.id": f"sr-cache-test-python-{uuid.uuid4()}",
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
    })
    c = Consumer(conf)
    c.subscribe([TOPIC])

    seen = []
    deadline = time.time() + timeout_s
    try:
        while time.time() < deadline and len(seen) < expected:
            msg = c.poll(2.0)
            if msg is None:
                continue
            if msg.error():
                raise KafkaException(msg.error())
            key = key_de(msg.key(), SerializationContext(TOPIC, MessageField.KEY))
            val = avro_de(msg.value(), SerializationContext(TOPIC, MessageField.VALUE))
            if val.get("client") != CLIENT_TAG or run_id not in val.get("msg", ""):
                continue
            seen.append((key, val))
    finally:
        c.close()

    if len(seen) < expected:
        print(f"[python] only saw {len(seen)}/{expected} of our run after {timeout_s}s", file=sys.stderr)
        sys.exit(3)
    print(f"[python] consumed {len(seen)} messages, all decoded correctly:")
    for k, v in seen:
        print(f"  key={k!r} value={v!r}")


def main():
    run_id = produce(3)
    consume(run_id, expected=3)
    print("[python] OK — Avro SerDes through the proxy succeeded")


if __name__ == "__main__":
    main()
