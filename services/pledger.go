package services

import (
	"context"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	ra "github.com/webtor-io/rest-api/services"
)

// ErrNoPledgerRecord: no pledger's uid has a tracker record for the private
// torrent. Its text is what resource.error shows.
var ErrNoPledgerRecord = errors.New("private: no pledger has a tracker record")

// listAsPledger lists the resource's files signed as each pledger in turn
// and returns the claims of the first one rest-api accepts: the run keeps
// them. Only a private_torrent refusal moves on to the next uid; any other
// error is returned as is. No uids means one attempt without a uid, as
// public resources always ran.
func listAsPledger(ctx context.Context, api *Api, id string, uids []string) (*Claims, []ra.ListItem, error) {
	if len(uids) == 0 {
		uids = []string{""}
	}
	for i, uid := range uids {
		cla := &Claims{Role: "vault", Uid: uid}
		items, err := listFiles(ctx, api, cla, id)
		if errors.Is(err, ErrPrivateTorrent) {
			if uid != "" {
				log.WithFields(log.Fields{"resource_id": id, "uid_index": i}).Info("private: pledger has no tracker record")
			}
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		return cla, items, nil
	}
	return nil, nil, ErrNoPledgerRecord
}

// listFiles pages through the resource's listing and keeps the files.
func listFiles(ctx context.Context, api *Api, cla *Claims, id string) ([]ra.ListItem, error) {
	args := &ListResourceContentArgs{Limit: 100}
	var items []ra.ListItem
	for {
		resp, err := api.ListResourceContent(ctx, cla, id, args)
		if err != nil {
			return nil, err
		}
		for _, it := range resp.Items {
			if it.Type == ra.ListTypeFile {
				items = append(items, it)
			}
		}
		if len(resp.Items) < int(args.Limit) {
			return items, nil
		}
		args.Offset += args.Limit
	}
}
