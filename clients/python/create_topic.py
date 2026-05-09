"""Idempotently create the test topic on Confluent Cloud."""
import os
from pathlib import Path
from confluent_kafka.admin import AdminClient, NewTopic


def load_env(path):
    for line in Path(path).read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        k, _, v = line.partition("=")
        os.environ.setdefault(k.strip(), v.strip())


def main():
    load_env(Path(__file__).resolve().parent.parent / ".env")
    topic = os.environ["TOPIC"]

    conf = {
        "bootstrap.servers": os.environ["BOOTSTRAP_SERVERS"],
        "security.protocol": "SASL_SSL",
        "sasl.mechanisms": "PLAIN",
        "sasl.username": os.environ["KAFKA_API_KEY"],
        "sasl.password": os.environ["KAFKA_API_SECRET"],
    }
    admin = AdminClient(conf)

    md = admin.list_topics(timeout=15)
    if topic in md.topics:
        print(f"topic {topic!r} already exists")
        return

    futures = admin.create_topics([NewTopic(topic, num_partitions=3, replication_factor=3)])
    for t, f in futures.items():
        try:
            f.result(timeout=30)
            print(f"topic {t!r} created")
        except Exception as e:
            print(f"create failed for {t!r}: {e}")
            raise


if __name__ == "__main__":
    main()
