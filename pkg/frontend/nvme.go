// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2022-2023 Dell Inc, or its subsidiaries.
// Copyright (C) 2023 Intel Corporation

// Package frontend implememnts the FrontEnd APIs (host facing) of the storage Server
package frontend

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"

	"github.com/opiproject/gospdk/spdk"
	pb "github.com/opiproject/opi-api/storage/v1alpha1/gen/go"
	"github.com/opiproject/opi-spdk-bridge/pkg/server"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	ipv4NvmeTCPProtocol = "ipv4"
	ipv6NvmeTCPProtocol = "ipv6"
)

// TODO: consider using https://pkg.go.dev/net#TCPAddr
type tcpSubsystemListener struct {
	listenAddr net.IP
	listenPort string
	protocol   string
}

func sortNvmeSubsystems(subsystems []*pb.NvmeSubsystem) {
	sort.Slice(subsystems, func(i int, j int) bool {
		return subsystems[i].Spec.Nqn < subsystems[j].Spec.Nqn
	})
}

func sortNvmeControllers(controllers []*pb.NvmeController) {
	sort.Slice(controllers, func(i int, j int) bool {
		return controllers[i].Spec.Name < controllers[j].Spec.Name
	})
}

func sortNvmeNamespaces(namespaces []*pb.NvmeNamespace) {
	sort.Slice(namespaces, func(i int, j int) bool {
		return namespaces[i].Spec.HostNsid < namespaces[j].Spec.HostNsid
	})
}

// NewTCPSubsystemListener creates a new instance of tcpSubsystemListener
func NewTCPSubsystemListener(listenAddr string) SubsystemListener {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		log.Panicf("Invalid ip:port tuple: %v", listenAddr)
	}

	parsedAddr := net.ParseIP(host)
	if parsedAddr == nil {
		log.Panicf("Invalid ip address: %v", host)
	}

	var protocol string
	switch {
	case parsedAddr.To4() != nil:
		protocol = ipv4NvmeTCPProtocol
	case parsedAddr.To16() != nil:
		protocol = ipv6NvmeTCPProtocol
	default:
		log.Panicf("Not supported protocol for: %v", listenAddr)
	}

	return &tcpSubsystemListener{
		listenAddr: parsedAddr,
		listenPort: port,
		protocol:   protocol,
	}
}

func (c *tcpSubsystemListener) Params(_ *pb.NvmeController, nqn string) spdk.NvmfSubsystemAddListenerParams {
	result := spdk.NvmfSubsystemAddListenerParams{}
	result.Nqn = nqn
	result.ListenAddress.Trtype = "tcp"
	result.ListenAddress.Traddr = c.listenAddr.String()
	result.ListenAddress.Trsvcid = c.listenPort
	result.ListenAddress.Adrfam = c.protocol

	return result
}

// CreateNvmeSubsystem creates an Nvme Subsystem
func (s *Server) CreateNvmeSubsystem(_ context.Context, in *pb.CreateNvmeSubsystemRequest) (*pb.NvmeSubsystem, error) {
	log.Printf("CreateNvmeSubsystem: Received from client: %v", in)
	// see https://google.aip.dev/133#user-specified-ids
	name := uuid.New().String()
	if in.NvmeSubsystemId != "" {
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.NvmeSubsystemId, in.NvmeSubsystem.Spec.Name)
		name = in.NvmeSubsystemId
	}
	in.NvmeSubsystem.Spec.Name = name
	// idempotent API when called with same key, should return same object
	subsys, ok := s.Nvme.Subsystems[in.NvmeSubsystem.Spec.Name]
	if ok {
		log.Printf("Already existing NvmeSubsystem with id %v", in.NvmeSubsystem.Spec.Name)
		return subsys, nil
	}
	// not found, so create a new one
	params := spdk.NvmfCreateSubsystemParams{
		Nqn:           in.NvmeSubsystem.Spec.Nqn,
		SerialNumber:  in.NvmeSubsystem.Spec.SerialNumber,
		ModelNumber:   in.NvmeSubsystem.Spec.ModelNumber,
		AllowAnyHost:  true,
		MaxNamespaces: int(in.NvmeSubsystem.Spec.MaxNamespaces),
	}
	var result spdk.NvmfCreateSubsystemResult
	err := s.rpc.Call("nvmf_create_subsystem", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not create NQN: %s", in.NvmeSubsystem.Spec.Nqn)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	var ver spdk.GetVersionResult
	err = s.rpc.Call("spdk_get_version", nil, &ver)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", ver)
	response := server.ProtoClone(in.NvmeSubsystem)
	response.Status = &pb.NvmeSubsystemStatus{FirmwareRevision: ver.Version}
	s.Nvme.Subsystems[in.NvmeSubsystem.Spec.Name] = response
	return response, nil
}

// DeleteNvmeSubsystem deletes an Nvme Subsystem
func (s *Server) DeleteNvmeSubsystem(_ context.Context, in *pb.DeleteNvmeSubsystemRequest) (*emptypb.Empty, error) {
	log.Printf("DeleteNvmeSubsystem: Received from client: %v", in)
	subsys, ok := s.Nvme.Subsystems[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	params := spdk.NvmfDeleteSubsystemParams{
		Nqn: subsys.Spec.Nqn,
	}
	var result spdk.NvmfDeleteSubsystemResult
	err := s.rpc.Call("nvmf_delete_subsystem", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not delete NQN: %s", subsys.Spec.Nqn)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	delete(s.Nvme.Subsystems, subsys.Spec.Name)
	return &emptypb.Empty{}, nil
}

// UpdateNvmeSubsystem updates an Nvme Subsystem
func (s *Server) UpdateNvmeSubsystem(_ context.Context, in *pb.UpdateNvmeSubsystemRequest) (*pb.NvmeSubsystem, error) {
	log.Printf("UpdateNvmeSubsystem: Received from client: %v", in)
	return nil, status.Errorf(codes.Unimplemented, "UpdateNvmeSubsystem method is not implemented")
}

// ListNvmeSubsystems lists Nvme Subsystems
func (s *Server) ListNvmeSubsystems(_ context.Context, in *pb.ListNvmeSubsystemsRequest) (*pb.ListNvmeSubsystemsResponse, error) {
	log.Printf("ListNvmeSubsystems: Received from client: %v", in)
	size, offset, perr := server.ExtractPagination(in.PageSize, in.PageToken, s.Pagination)
	if perr != nil {
		log.Printf("error: %v", perr)
		return nil, perr
	}
	var result []spdk.NvmfGetSubsystemsResult
	err := s.rpc.Call("nvmf_get_subsystems", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	token := ""
	log.Printf("Limiting result len(%d) to [%d:%d]", len(result), offset, size)
	result, hasMoreElements := server.LimitPagination(result, offset, size)
	if hasMoreElements {
		token = uuid.New().String()
		s.Pagination[token] = offset + size
	}
	Blobarray := make([]*pb.NvmeSubsystem, len(result))
	for i := range result {
		r := &result[i]
		Blobarray[i] = &pb.NvmeSubsystem{Spec: &pb.NvmeSubsystemSpec{Nqn: r.Nqn, SerialNumber: r.SerialNumber, ModelNumber: r.ModelNumber}}
	}
	sortNvmeSubsystems(Blobarray)
	return &pb.ListNvmeSubsystemsResponse{NvmeSubsystems: Blobarray, NextPageToken: token}, nil
}

// GetNvmeSubsystem gets Nvme Subsystems
func (s *Server) GetNvmeSubsystem(_ context.Context, in *pb.GetNvmeSubsystemRequest) (*pb.NvmeSubsystem, error) {
	log.Printf("GetNvmeSubsystem: Received from client: %v", in)
	subsys, ok := s.Nvme.Subsystems[in.Name]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}

	var result []spdk.NvmfGetSubsystemsResult
	err := s.rpc.Call("nvmf_get_subsystems", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)

	for i := range result {
		r := &result[i]
		if r.Nqn == subsys.Spec.Nqn {
			return &pb.NvmeSubsystem{Spec: &pb.NvmeSubsystemSpec{Nqn: r.Nqn, SerialNumber: r.SerialNumber, ModelNumber: r.ModelNumber}, Status: &pb.NvmeSubsystemStatus{FirmwareRevision: "TBD"}}, nil
		}
	}
	msg := fmt.Sprintf("Could not find NQN: %s", subsys.Spec.Nqn)
	log.Print(msg)
	return nil, status.Errorf(codes.InvalidArgument, msg)
}

// NvmeSubsystemStats gets Nvme Subsystem stats
func (s *Server) NvmeSubsystemStats(_ context.Context, in *pb.NvmeSubsystemStatsRequest) (*pb.NvmeSubsystemStatsResponse, error) {
	log.Printf("NvmeSubsystemStats: Received from client: %v", in)
	var result spdk.NvmfGetSubsystemStatsResult
	err := s.rpc.Call("nvmf_get_stats", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	return &pb.NvmeSubsystemStatsResponse{Stats: &pb.VolumeStats{ReadOpsCount: -1, WriteOpsCount: -1}}, nil
}

// CreateNvmeController creates an Nvme controller
func (s *Server) CreateNvmeController(_ context.Context, in *pb.CreateNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("Received from client: %v", in.NvmeController)
	// check input parameters validity
	if in.NvmeController.Spec == nil || in.NvmeController.Spec.SubsystemId == nil || in.NvmeController.Spec.SubsystemId.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid input subsystem parameters")
	}
	// see https://google.aip.dev/133#user-specified-ids
	name := uuid.New().String()
	if in.NvmeControllerId != "" {
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.NvmeControllerId, in.NvmeController.Spec.Name)
		name = in.NvmeControllerId
	}
	in.NvmeController.Spec.Name = name
	// idempotent API when called with same key, should return same object
	controller, ok := s.Nvme.Controllers[in.NvmeController.Spec.Name]
	if ok {
		log.Printf("Already existing NvmeController with id %v", in.NvmeController.Spec.Name)
		return controller, nil
	}
	// not found, so create a new one
	subsys, ok := s.Nvme.Subsystems[in.NvmeController.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", in.NvmeController.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := s.Nvme.subsysListener.Params(in.NvmeController, subsys.Spec.Nqn)
	var result spdk.NvmfSubsystemAddListenerResult
	err := s.rpc.Call("nvmf_subsystem_add_listener", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not create CTRL: %s", in.NvmeController.Spec.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	s.Nvme.Controllers[in.NvmeController.Spec.Name] = in.NvmeController
	s.Nvme.Controllers[in.NvmeController.Spec.Name].Spec.NvmeControllerId = -1
	s.Nvme.Controllers[in.NvmeController.Spec.Name].Status = &pb.NvmeControllerStatus{Active: true}
	response := server.ProtoClone(in.NvmeController)

	return response, nil
}

// DeleteNvmeController deletes an Nvme controller
func (s *Server) DeleteNvmeController(_ context.Context, in *pb.DeleteNvmeControllerRequest) (*emptypb.Empty, error) {
	log.Printf("Received from client: %v", in.Name)
	controller, ok := s.Nvme.Controllers[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	subsys, ok := s.Nvme.Subsystems[controller.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", controller.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := s.Nvme.subsysListener.Params(controller, subsys.Spec.Nqn)
	var result spdk.NvmfSubsystemAddListenerResult
	err := s.rpc.Call("nvmf_subsystem_remove_listener", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not delete NQN:ID %s:%d", subsys.Spec.Nqn, controller.Spec.NvmeControllerId)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	delete(s.Nvme.Controllers, controller.Spec.Name)
	return &emptypb.Empty{}, nil
}

// UpdateNvmeController updates an Nvme controller
func (s *Server) UpdateNvmeController(_ context.Context, in *pb.UpdateNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("UpdateNvmeController: Received from client: %v", in)
	s.Nvme.Controllers[in.NvmeController.Spec.Name] = in.NvmeController
	s.Nvme.Controllers[in.NvmeController.Spec.Name].Status = &pb.NvmeControllerStatus{Active: true}
	response := server.ProtoClone(in.NvmeController)
	return response, nil
}

// ListNvmeControllers lists Nvme controllers
func (s *Server) ListNvmeControllers(_ context.Context, in *pb.ListNvmeControllersRequest) (*pb.ListNvmeControllersResponse, error) {
	log.Printf("Received from client: %v", in.Parent)
	Blobarray := []*pb.NvmeController{}
	for _, controller := range s.Nvme.Controllers {
		Blobarray = append(Blobarray, controller)
	}
	sortNvmeControllers(Blobarray)
	token := uuid.New().String()
	s.Pagination[token] = int(in.PageSize)
	return &pb.ListNvmeControllersResponse{NvmeControllers: Blobarray, NextPageToken: token}, nil
}

// GetNvmeController gets an Nvme controller
func (s *Server) GetNvmeController(_ context.Context, in *pb.GetNvmeControllerRequest) (*pb.NvmeController, error) {
	log.Printf("Received from client: %v", in.Name)
	controller, ok := s.Nvme.Controllers[in.Name]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	return &pb.NvmeController{Spec: &pb.NvmeControllerSpec{Name: in.Name, NvmeControllerId: controller.Spec.NvmeControllerId}, Status: &pb.NvmeControllerStatus{Active: true}}, nil
}

// NvmeControllerStats gets an Nvme controller stats
func (s *Server) NvmeControllerStats(_ context.Context, in *pb.NvmeControllerStatsRequest) (*pb.NvmeControllerStatsResponse, error) {
	log.Printf("NvmeControllerStats: Received from client: %v", in)
	return &pb.NvmeControllerStatsResponse{Stats: &pb.VolumeStats{ReadOpsCount: -1, WriteOpsCount: -1}}, nil
}

// CreateNvmeNamespace creates an Nvme namespace
func (s *Server) CreateNvmeNamespace(_ context.Context, in *pb.CreateNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("CreateNvmeNamespace: Received from client: %v", in)
	// check input parameters validity
	if in.NvmeNamespace.Spec == nil || in.NvmeNamespace.Spec.SubsystemId == nil || in.NvmeNamespace.Spec.SubsystemId.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid input subsystem parameters")
	}
	// see https://google.aip.dev/133#user-specified-ids
	name := uuid.New().String()
	if in.NvmeNamespaceId != "" {
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.NvmeNamespaceId, in.NvmeNamespace.Spec.Name)
		name = in.NvmeNamespaceId
	}
	in.NvmeNamespace.Spec.Name = name
	// idempotent API when called with same key, should return same object
	namespace, ok := s.Nvme.Namespaces[in.NvmeNamespace.Spec.Name]
	if ok {
		log.Printf("Already existing NvmeNamespace with id %v", in.NvmeNamespace.Spec.Name)
		return namespace, nil
	}
	// not found, so create a new one
	subsys, ok := s.Nvme.Subsystems[in.NvmeNamespace.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", in.NvmeNamespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := spdk.NvmfSubsystemAddNsParams{
		Nqn: subsys.Spec.Nqn,
	}

	// TODO: using bdev for volume id as a middle end handle for now
	params.Namespace.Nsid = int(in.NvmeNamespace.Spec.HostNsid)
	params.Namespace.BdevName = in.NvmeNamespace.Spec.VolumeId.Value

	var result spdk.NvmfSubsystemAddNsResult
	err := s.rpc.Call("nvmf_subsystem_add_ns", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if result < 0 {
		msg := fmt.Sprintf("Could not create NS: %s", in.NvmeNamespace.Spec.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	s.Nvme.Namespaces[in.NvmeNamespace.Spec.Name] = in.NvmeNamespace

	response := server.ProtoClone(in.NvmeNamespace)
	response.Status = &pb.NvmeNamespaceStatus{PciState: 2, PciOperState: 1}
	return response, nil
}

// DeleteNvmeNamespace deletes an Nvme namespace
func (s *Server) DeleteNvmeNamespace(_ context.Context, in *pb.DeleteNvmeNamespaceRequest) (*emptypb.Empty, error) {
	log.Printf("DeleteNvmeNamespace: Received from client: %v", in)
	namespace, ok := s.Nvme.Namespaces[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	subsys, ok := s.Nvme.Subsystems[namespace.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", namespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	params := spdk.NvmfSubsystemRemoveNsParams{
		Nqn:  subsys.Spec.Nqn,
		Nsid: int(namespace.Spec.HostNsid),
	}
	var result spdk.NvmfSubsystemRemoveNsResult
	err := s.rpc.Call("nvmf_subsystem_remove_ns", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not delete NS: %s", in.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	delete(s.Nvme.Namespaces, namespace.Spec.Name)
	return &emptypb.Empty{}, nil
}

// UpdateNvmeNamespace updates an Nvme namespace
func (s *Server) UpdateNvmeNamespace(_ context.Context, in *pb.UpdateNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("UpdateNvmeNamespace: Received from client: %v", in)
	s.Nvme.Namespaces[in.NvmeNamespace.Spec.Name] = in.NvmeNamespace
	s.Nvme.Namespaces[in.NvmeNamespace.Spec.Name].Status = &pb.NvmeNamespaceStatus{PciState: 2, PciOperState: 1}

	response := server.ProtoClone(in.NvmeNamespace)
	return response, nil
}

// ListNvmeNamespaces lists Nvme namespaces
func (s *Server) ListNvmeNamespaces(_ context.Context, in *pb.ListNvmeNamespacesRequest) (*pb.ListNvmeNamespacesResponse, error) {
	log.Printf("ListNvmeNamespaces: Received from client: %v", in)
	size, offset, perr := server.ExtractPagination(in.PageSize, in.PageToken, s.Pagination)
	if perr != nil {
		log.Printf("error: %v", perr)
		return nil, perr
	}
	nqn := ""
	if in.Parent != "" {
		subsys, ok := s.Nvme.Subsystems[in.Parent]
		if !ok {
			err := fmt.Errorf("unable to find subsystem %s", in.Parent)
			log.Printf("error: %v", err)
			return nil, err
		}
		nqn = subsys.Spec.Nqn
	}
	var result []spdk.NvmfGetSubsystemsResult
	err := s.rpc.Call("nvmf_get_subsystems", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	token := ""
	Blobarray := []*pb.NvmeNamespace{}
	for i := range result {
		rr := &result[i]
		if rr.Nqn == nqn || nqn == "" {
			log.Printf("Limiting result len(%d) to [%d:%d]", len(result), offset, size)
			hasMoreElements := false
			rr.Namespaces, hasMoreElements = server.LimitPagination(rr.Namespaces, offset, size)
			if hasMoreElements {
				token = uuid.New().String()
				s.Pagination[token] = offset + size
			}
			for j := range rr.Namespaces {
				r := &rr.Namespaces[j]
				Blobarray = append(Blobarray, &pb.NvmeNamespace{Spec: &pb.NvmeNamespaceSpec{HostNsid: int32(r.Nsid)}})
			}
		}
	}
	if len(Blobarray) > 0 {
		sortNvmeNamespaces(Blobarray)
		return &pb.ListNvmeNamespacesResponse{NvmeNamespaces: Blobarray, NextPageToken: token}, nil
	}

	msg := fmt.Sprintf("Could not find any namespaces for NQN: %s", nqn)
	log.Print(msg)
	return nil, status.Errorf(codes.InvalidArgument, msg)
}

// GetNvmeNamespace gets an Nvme namespace
func (s *Server) GetNvmeNamespace(_ context.Context, in *pb.GetNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("GetNvmeNamespace: Received from client: %v", in)
	namespace, ok := s.Nvme.Namespaces[in.Name]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	// TODO: do we even query SPDK to confirm if namespace is present?
	// return namespace, nil

	// fetch subsystems -> namespaces from Server, match the nsid to find the corresponding namespace
	subsys, ok := s.Nvme.Subsystems[namespace.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", namespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	var result []spdk.NvmfGetSubsystemsResult
	err := s.rpc.Call("nvmf_get_subsystems", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	for i := range result {
		rr := &result[i]
		if rr.Nqn == subsys.Spec.Nqn {
			for j := range rr.Namespaces {
				r := &rr.Namespaces[j]
				if int32(r.Nsid) == namespace.Spec.HostNsid {
					return &pb.NvmeNamespace{
						Spec:   &pb.NvmeNamespaceSpec{Name: namespace.Spec.Name, HostNsid: namespace.Spec.HostNsid},
						Status: &pb.NvmeNamespaceStatus{PciState: 2, PciOperState: 1},
					}, nil
				}
			}
			msg := fmt.Sprintf("Could not find NSID: %d", namespace.Spec.HostNsid)
			log.Print(msg)
			return nil, status.Errorf(codes.InvalidArgument, msg)
		}
	}
	msg := fmt.Sprintf("Could not find NQN: %s", subsys.Spec.Nqn)
	log.Print(msg)
	return nil, status.Errorf(codes.InvalidArgument, msg)
}

// NvmeNamespaceStats gets an Nvme namespace stats
func (s *Server) NvmeNamespaceStats(_ context.Context, in *pb.NvmeNamespaceStatsRequest) (*pb.NvmeNamespaceStatsResponse, error) {
	log.Printf("NvmeNamespaceStats: Received from client: %v", in)
	return &pb.NvmeNamespaceStatsResponse{Stats: &pb.VolumeStats{ReadOpsCount: -1, WriteOpsCount: -1}}, nil
}
