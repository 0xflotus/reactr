package api

import (
	"encoding/json"

	"github.com/pkg/errors"
	"github.com/suborbital/reactr/rwasm/runtime"
)

func GraphQLQueryHandler() runtime.HostFn {
	fn := func(args ...interface{}) (interface{}, error) {
		endpointPointer := args[0].(int32)
		endpointSize := args[1].(int32)
		queryPointer := args[2].(int32)
		querySize := args[3].(int32)
		ident := args[4].(int32)

		ret := graphql_query(endpointPointer, endpointSize, queryPointer, querySize, ident)

		return ret, nil
	}

	return runtime.NewHostFn("graphql_query", 5, true, fn)
}

func graphql_query(endpointPointer int32, endpointSize int32, queryPointer int32, querySize int32, identifier int32) int32 {
	inst, err := runtime.InstanceForIdentifier(identifier, true)
	if err != nil {
		runtime.InternalLogger().Error(errors.Wrap(err, "[rwasm] alert: invalid identifier used, potential malicious activity"))
		return -1
	}

	endpointBytes := inst.ReadMemory(endpointPointer, endpointSize)
	endpoint := string(endpointBytes)

	queryBytes := inst.ReadMemory(queryPointer, querySize)
	query := string(queryBytes)

	resp, err := inst.Ctx().GraphQLClient.Do(inst.Ctx().Auth, endpoint, query)
	if err != nil {
		runtime.InternalLogger().Error(errors.Wrap(err, "failed to GraphQLClient.Do"))
		return -1
	}

	respBytes, err := json.Marshal(resp)
	if err != nil {
		runtime.InternalLogger().Error(errors.Wrap(err, "[rwasm] alert: failed to Marshal"))
		return -1
	}

	inst.SetFFIResult(respBytes)

	return int32(len(respBytes))
}
