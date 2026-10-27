package cluster

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/jackc/pglogrepl"
)

// pgoutput protocol v1 messages (see the Postgres "Logical Replication Message Formats")

func testRelationMessage(id uint32, name string, columns ...string) []byte {
	b := []byte{'R'}
	b = binary.BigEndian.AppendUint32(b, id)
	b = append(b, "public\x00"...)
	b = append(b, name+"\x00"...)
	b = append(b, 'f') // replica identity full
	b = binary.BigEndian.AppendUint16(b, uint16(len(columns)))
	for _, col := range columns {
		b = append(b, 0) // flags
		b = append(b, col+"\x00"...)
		b = binary.BigEndian.AppendUint32(b, 25) // text
		b = binary.BigEndian.AppendUint32(b, 0xFFFFFFFF)
	}
	return b
}

func testBeginMessage(finalLSN uint64) []byte {
	b := []byte{'B'}
	b = binary.BigEndian.AppendUint64(b, finalLSN)
	b = binary.BigEndian.AppendUint64(b, 0)
	b = binary.BigEndian.AppendUint32(b, 1)
	return b
}

func testCommitMessage(commitLSN uint64, endLSN uint64) []byte {
	b := []byte{'C', 0}
	b = binary.BigEndian.AppendUint64(b, commitLSN)
	b = binary.BigEndian.AppendUint64(b, endLSN)
	b = binary.BigEndian.AppendUint64(b, 0)
	return b
}

func testTupleData(values ...string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(values)))
	for _, v := range values {
		b = append(b, 't')
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = append(b, v...)
	}
	return b
}

func testInsertMessage(relationId uint32, values ...string) []byte {
	b := []byte{'I'}
	b = binary.BigEndian.AppendUint32(b, relationId)
	b = append(b, 'N')
	return append(b, testTupleData(values...)...)
}

func testDeleteMessage(relationId uint32, values ...string) []byte {
	b := []byte{'D'}
	b = binary.BigEndian.AppendUint32(b, relationId)
	b = append(b, 'O')
	return append(b, testTupleData(values...)...)
}

func TestReplicationDecoder(t *testing.T) {
	t.Parallel()

	d := &replicationDecoder{
		relations:         map[uint32]*pglogrepl.RelationMessage{},
		resolveCollection: func(table string) string { return "id_" + table },
		maxEvents:         3,
	}

	decode := func(data []byte) (bool, pglogrepl.LSN) {
		t.Helper()
		committed, endLSN, err := d.decode(data)
		if err != nil {
			t.Fatal(err)
		}
		return committed, endLSN
	}

	decode(testRelationMessage(1, "posts", "id", "title"))
	decode(testBeginMessage(0xABC))

	if !d.inTransaction {
		t.Fatal("Expected to be in a transaction")
	}

	// the events are taken as soon as they are decoded (before the commit)
	if committed, _ := decode(testInsertMessage(1, "r1", "a")); committed {
		t.Fatal("Didn't expect a commit")
	}
	events := d.take()
	if len(events) != 1 || events[0].Id != "0000000000000ABC_000001" || events[0].Op != "create" ||
		events[0].RecordId != "r1" || events[0].CollectionId != "id_posts" || events[0].Snapshot != "" {
		t.Fatalf("Unexpected first event %+v", events)
	}
	if events := d.take(); len(events) != 0 {
		t.Fatalf("Expected the events to be taken once, got %d", len(events))
	}

	decode(testInsertMessage(1, "r2", "b"))
	decode(testDeleteMessage(1, "r3", "c"))
	// above maxEvents
	decode(testInsertMessage(1, "r4", "d"))
	decode(testInsertMessage(1, "r5", "e"))

	events = d.take()
	ids := []string{}
	for _, e := range events {
		ids = append(ids, e.Id+"/"+e.Op+"/"+e.RecordId)
	}
	expected := []string{"0000000000000ABC_000002/create/r2", "0000000000000ABC_000003/delete/r3"}
	if fmt.Sprint(ids) != fmt.Sprint(expected) {
		t.Fatalf("Expected events %v, got %v", expected, ids)
	}
	if events[1].Snapshot != `{"id":"r3","title":"c"}` {
		t.Fatalf("Unexpected delete snapshot %s", events[1].Snapshot)
	}

	committed, endLSN := decode(testCommitMessage(0xABC, 0xAF0))
	if !committed || endLSN != 0xAF0 || d.inTransaction {
		t.Fatalf("Expected the commit with end LSN 0xAF0, got %v %X", committed, endLSN)
	}
	if d.total != 3 || d.skipped != 2 {
		t.Fatalf("Expected 3 events and 2 skipped, got %d and %d", d.total, d.skipped)
	}

	// the next transaction starts with new counters
	decode(testBeginMessage(0xB00))
	decode(testInsertMessage(1, "r6", "f"))
	if events := d.take(); len(events) != 1 || events[0].Id != "0000000000000B00_000001" {
		t.Fatalf("Unexpected event of the next transaction %+v", events)
	}
	if d.total != 1 || d.skipped != 0 {
		t.Fatalf("Expected the counters to be reset, got %d and %d", d.total, d.skipped)
	}
}
