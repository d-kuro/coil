package coild

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/cybozu-go/coil"
	"github.com/cybozu-go/coil/model"
	"github.com/cybozu-go/log"
	"github.com/cybozu-go/well"
)

type addressInfo struct {
	Address string `json:"address"`
	Status  int    `json:"status"`
}

func (s *Server) determinePoolName(ctx context.Context, podNS string) (string, error) {
	_, err := s.db.GetPool(ctx, podNS)
	switch err {
	case nil:
		return podNS, nil
	case model.ErrNotFound:
		return "default", nil
	default:
		return "", err
	}
}

func (s *Server) getAllocatedIP(ctx context.Context, containerID string) (net.IP, error) {
}

func (s *Server) handleNewIP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		renderError(r.Context(), w, APIErrBadMethod)
		return
	}

	input := struct {
		PodNS       string `json:"pod-namespace"`
		PodName     string `json:"pod-name"`
		ContainerID string `json:"container-id"`
		AddressType string `json:"address-type"`
	}{}

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		renderError(r.Context(), w, BadRequest(err.Error()))
		return
	}
	if len(input.PodNS) == 0 {
		renderError(r.Context(), w, BadRequest("no pod namespace"))
		return
	}
	if len(input.PodName) == 0 {
		renderError(r.Context(), w, BadRequest("no pod name"))
		return
	}
	if len(input.ContainerID) == 0 {
		renderError(r.Context(), w, BadRequest("no container-id"))
		return
	}

	poolName, err := s.determinePoolName(r.Context(), input.PodNS)
	if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	containerID := input.ContainerID
	_, err := s.getAllocatedIP(r.Context(), containerID)
	if err == nil {
		renderError(r.Context(), w, APIErrConflict)
		return
	} else if err != ErrNotFound {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	blocks, err := s.db.GetMyBlocks(r.Context(), s.nodeName)
	if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}
	bl := blocks[poolName]

	assignment := coil.IPAssignment{
		ContainerID: containerID,
		Namespace:   input.PodNS,
		Pod:         input.PodName,
		CreatedAt:   time.Now().UTC(),
	}
RETRY:
	fields := well.FieldsFromContext(r.Context())
	for _, block := range bl {
		ip, err := s.db.AllocateIP(r.Context(), block, assignment)
		if err == model.ErrBlockIsFull {
			continue
		}
		if err != nil {
			renderError(r.Context(), w, InternalServerError(err))
			return
		}

		resp := addressInfo{
			Address: ip.String(),
			Status:  http.StatusOK,
		}
		renderJSON(w, resp, http.StatusOK)

		fields["namespace"] = input.PodNS
		fields["pod"] = input.PodName
		fields["containerid"] = containerID
		fields["pool"] = poolName
		fields["ip"] = ip.String()
		log.Info("allocate an address", fields)
		return
	}

	block, err := s.db.AcquireBlock(r.Context(), s.nodeName, poolName)
	fields["pool"] = poolName
	switch err {
	case model.ErrOutOfBlocks:
		fields[log.FnError] = err
		log.Error("no more blocks in pool", fields)
		renderError(r.Context(), w, APIError{
			Status:  http.StatusServiceUnavailable,
			Message: "no more blocks in pool " + poolName,
			Err:     err,
		})
		return
	case model.ErrNotFound:
		fields[log.FnError] = err
		log.Error("address pool is not found", fields)
		renderError(r.Context(), w, APIError{
			Status:  http.StatusInternalServerError,
			Message: "address pool is not found " + poolName,
			Err:     err,
		})
		return
	case nil:
		// nothing to do
	default:
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	fields["block"] = block.String()
	log.Info("acquired new block", fields)

	if !s.dryRun {
		err = addBlockRouting(s.tableID, s.protocolID, block)
		if err != nil {
			fields[log.FnError] = err
			log.Critical("failed to add a block to routing table", fields)
			renderError(r.Context(), w, InternalServerError(err))
			return
		}
	}

	newAddressBlocks := make([]*net.IPNet, len(bl)+1)
	newAddressBlocks[0] = block
	copy(newAddressBlocks[1:], bl)
	bl = newAddressBlocks
	goto RETRY
}

func (s *Server) handleIPGet(w http.ResponseWriter, r *http.Request, containerID string) {
	ip, err := s.getAllocatedIP(r.Context(), containerID)
	if err == ErrNotFound {
		renderError(r.Context(), w, APIErrNotFound)
		return
	} else if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	resp := addressInfo{
		Address: ip.String(),
		Status:  http.StatusOK,
	}

	renderJSON(w, resp, http.StatusOK)
}

func (s *Server) handleIPDelete(w http.ResponseWriter, r *http.Request, keys []string) {
	containerID := keys[2]
	respNotFoundOK := addressInfo{
		Address: "",
		Status:  http.StatusOK,
	}

	// Get my blocks first because IP and block may be freed by coil-controller after getAllocatedIP(), which will cause false-evaluation of orphaned IP.
	blocks, err := s.db.GetMyBlocks(r.Context(), s.nodeName)
	if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	// In older than version 1.0.2 namespace and pod name are stored in DB.  We cannot find such entry.  coil-controller will delete it later.
	ip, err := s.getAllocatedIP(r.Context(), containerID)
	if err == ErrNotFound {
		renderJSON(w, respNotFoundOK, http.StatusOK)
		return
	} else if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	var block *net.IPNet
OUTER:
	for _, bl := range blocks {
		for _, b := range bl {
			if b.Contains(ip) {
				block = b
				break OUTER
			}
		}
	}

	fields := well.FieldsFromContext(r.Context())
	if block == nil {
		fields["ip"] = ip.String()
		log.Critical("orphaned IP address", fields)
		renderError(r.Context(), w, InternalServerError(errors.New("orphaned IP address")))
		return
	}

	assignment, modRev, err := s.db.GetAddressInfo(r.Context(), ip)
	if err == ErrNotFound {
		renderJSON(w, respNotFoundOK, http.StatusOK)
		return
	} else if err != nil {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	if assignment.ContainerID != containerID {
		renderJSON(w, respNotFoundOK, http.StatusOK)
		return
	}

	err := s.db.FreeIP(r.Context(), block, ip, modRev)
	if err != nil && err != model.ErrModRevDiffers {
		renderError(r.Context(), w, InternalServerError(err))
		return
	}

	resp := addressInfo{
		Address: ip.String(),
		Status:  http.StatusOK,
	}

	renderJSON(w, resp, http.StatusOK)

	fields["namespace"] = keys[0]
	fields["pod"] = keys[1]
	fields["containerid"] = keys[2]
	fields["ip"] = ip.String()
	log.Info("free an address", fields)
}
