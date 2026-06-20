package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// GRPCServer exposes Huginn's portfolio and strategy state over gRPC for
// programmatic access at lower latency than the HTTP API.
type GRPCServer struct {
	snapshotFn func() map[string]interface{}
	port       string
	srv        *grpc.Server
}

var (
	protoOnce    sync.Once
	responseDesc protoreflect.MessageDescriptor
)

func ensureProtoRegistered() {
	protoOnce.Do(func() {
		fdp := &descriptorpb.FileDescriptorProto{
			Name:       proto.String("huginn/v1/service.proto"),
			Package:    proto.String("huginn"),
			Syntax:     proto.String("proto3"),
			Dependency: []string{"google/protobuf/empty.proto"},
			MessageType: []*descriptorpb.DescriptorProto{
				{
					Name: proto.String("SnapshotResponse"),
					Field: []*descriptorpb.FieldDescriptorProto{
						{
							Name:     proto.String("json_payload"),
							Number:   proto.Int32(1),
							Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
							Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
							JsonName: proto.String("jsonPayload"),
						},
						{
							Name:     proto.String("status_code"),
							Number:   proto.Int32(2),
							Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
							Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
							JsonName: proto.String("statusCode"),
						},
					},
				},
			},
			Service: []*descriptorpb.ServiceDescriptorProto{
				{
					Name: proto.String("HuginnService"),
					Method: []*descriptorpb.MethodDescriptorProto{
						{
							Name:       proto.String("GetSnapshot"),
							InputType:  proto.String(".google.protobuf.Empty"),
							OutputType: proto.String(".huginn.SnapshotResponse"),
						},
					},
				},
			},
		}

		fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
		if err != nil {
			panic(fmt.Sprintf("huginn: build proto file descriptor: %v", err))
		}
		if regErr := protoregistry.GlobalFiles.RegisterFile(fd); regErr != nil {
			slog.Warn("Proto file already registered", "error", regErr)
		}

		responseDesc = fd.Messages().ByName("SnapshotResponse")
		mt := dynamicpb.NewMessageType(responseDesc)
		if regErr := protoregistry.GlobalTypes.RegisterMessage(mt); regErr != nil {
			slog.Warn("Proto message type already registered", "error", regErr)
		}
	})
}

// huginnServiceServer is the gRPC service interface.
type huginnServiceServer interface {
	GetSnapshot(context.Context, *emptypb.Empty) (*dynamicpb.Message, error)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: "huginn.HuginnService",
	HandlerType: (*huginnServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetSnapshot",
			Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
				req := new(emptypb.Empty)
				if err := dec(req); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(huginnServiceServer).GetSnapshot(ctx, req)
				}
				info := &grpc.UnaryServerInfo{
					Server:     srv,
					FullMethod: "/huginn.HuginnService/GetSnapshot",
				}
				return interceptor(ctx, req, info, func(ctx context.Context, req interface{}) (interface{}, error) {
					return srv.(huginnServiceServer).GetSnapshot(ctx, req.(*emptypb.Empty))
				})
			},
		},
	},
	Streams: []grpc.StreamDesc{},
}

func NewGRPCServer(port string, snapshotFn func() map[string]interface{}) *GRPCServer {
	ensureProtoRegistered()
	return &GRPCServer{
		snapshotFn: snapshotFn,
		port:       port,
	}
}

func (s *GRPCServer) GetSnapshot(_ context.Context, _ *emptypb.Empty) (*dynamicpb.Message, error) {
	data := s.snapshotFn()
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}

	resp := dynamicpb.NewMessage(responseDesc)
	resp.Set(responseDesc.Fields().ByName("json_payload"), protoreflect.ValueOfString(string(payload)))
	resp.Set(responseDesc.Fields().ByName("status_code"), protoreflect.ValueOfInt32(200))
	return resp, nil
}

func (s *GRPCServer) Start() error {
	lis, err := net.Listen("tcp", ":"+s.port)
	if err != nil {
		return fmt.Errorf("gRPC listen: %w", err)
	}

	s.srv = grpc.NewServer()
	s.srv.RegisterService(&serviceDesc, s)
	reflection.Register(s.srv)

	slog.Info("gRPC server started", "port", s.port)

	go func() {
		if err := s.srv.Serve(lis); err != nil {
			slog.Error("gRPC server error", "error", err)
		}
	}()

	return nil
}

func (s *GRPCServer) Stop() {
	if s.srv != nil {
		s.srv.GracefulStop()
		slog.Info("gRPC server stopped")
	}
}
