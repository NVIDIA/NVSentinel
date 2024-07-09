import click, grpc, time, random, string
from example_health_monitor.protos import (
    platformconnector_pb2_grpc,
    platformconnector_pb2,
)
from google.protobuf.timestamp_pb2 import Timestamp


@click.command()
@click.option(
    "--socket", "-s", default="/var/run/nvsentinel.sock", help="unix domain socket path"
)
def cli(socket):
    print("Hello, World")

    while True:
        with grpc.insecure_channel(f"unix://{socket}") as chan:
            stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
            print("Sending health event")

            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            events = []

            for i in range(random.randint(1, 5)):
                print(f"Generating health event {i}")

                isHealthy = True if random.random() < 0.5 else False
                version = 1
                agent = "example-health-monitor"
                checkName = "example-check"
                componentClass = "example-component"
                generatedTimestamp = timestamp
                isFatal = True if random.random() < 0.5 else False
                errorCode = (
                    (
                        "".join(
                            random.choice(string.ascii_letters + string.digits)
                            for i in range(5)
                        )
                    )
                    if not isHealthy
                    else ""
                )
                entitiesImpacted = (
                    [
                        ("".join(random.choice(string.digits) for i in range(5)))
                        for i in range(random.randint(1, 8))
                    ]
                    if not isHealthy
                    else []
                )
                message = "".join(
                    random.choice(string.ascii_letters) for i in range(15)
                )
                recommendedAction = (
                    platformconnector_pb2.APPLICATION_RESTART
                    if not isHealthy
                    else platformconnector_pb2.UNKNOWN
                )

                print(f"isHealthy={isHealthy}")
                print(f"version={version}")
                print(f"agent={agent}")
                print(f"checkName={checkName}")
                print(f"componentClass={componentClass}")
                print(f"generatedTimestamp={generatedTimestamp}")
                print(f"isFatal={isFatal}")
                print(f"errorCode={errorCode}")
                print(f"entitiesImpacted={entitiesImpacted}")
                print(f"message={message}")
                print(f"recommendedAction={recommendedAction}")

                events.append(
                    platformconnector_pb2.HealthEvent(
                        version=version,
                        agent=agent,
                        checkName=checkName,
                        componentClass=componentClass,
                        generatedTimestamp=generatedTimestamp,
                        isFatal=isFatal,
                        isHealthy=isHealthy,
                        errorCode=errorCode,
                        entitiesImpacted=entitiesImpacted,
                        message=message,
                        recommendedAction=recommendedAction,
                    )
                )
                print("-" * 80)

            stub.HealthEventOccuredV1(
                platformconnector_pb2.HealthEvents(events=events, version=1)
            )
            print("=" * 80)
            print("Sleeping...")
            time.sleep(10)


if __name__ == "__main__":
    cli()
