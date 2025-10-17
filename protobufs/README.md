# Protobufs

Protocol buffers shared between health monitors and platform connectors

Run the below command in the root directory of nvsentinel to generate the grpc files for platformconnector,syslog-health-monitor
protoc -I protobufs/ --go_out=platform-connectors/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/protos/ protobufs/platformconnector.proto
protoc -I protobufs/ --go_out=health-monitors/syslog-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/syslog-health-monitor/pkg/protos/ protobufs/platformconnector.proto
