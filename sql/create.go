// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/security"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/sql/privilege"
	"github.com/cockroachdb/cockroach/sql/sqlbase"
	"github.com/cockroachdb/cockroach/util"
	"github.com/pkg/errors"
)

type createDatabaseNode struct {
	p *planner
	n *parser.CreateDatabase
}

// CreateDatabase creates a database.
// Privileges: security.RootUser user.
//   Notes: postgres requires superuser or "CREATEDB".
//          mysql uses the mysqladmin command.
func (p *planner) CreateDatabase(n *parser.CreateDatabase) (planNode, error) {
	if n.Name == "" {
		return nil, errEmptyDatabaseName
	}

	if n.Encoding != nil {
		encoding, err := n.Encoding.ResolveAsType(&p.semaCtx, parser.TypeString)
		if err != nil {
			return nil, err
		}
		encodingStr := string(*encoding.(*parser.DString))
		// We only support UTF8 (and aliases for UTF8).
		if !(strings.EqualFold(encodingStr, "UTF8") ||
			strings.EqualFold(encodingStr, "UTF-8") ||
			strings.EqualFold(encodingStr, "UNICODE")) {
			return nil, fmt.Errorf("%s is not a supported encoding", encoding)
		}
	}

	if p.session.User != security.RootUser {
		return nil, errors.Errorf("only %s is allowed to create databases", security.RootUser)
	}

	return &createDatabaseNode{p: p, n: n}, nil
}

func (n *createDatabaseNode) expandPlan() error {
	return nil
}

func (n *createDatabaseNode) Start() error {
	desc := makeDatabaseDesc(n.n)

	created, err := n.p.createDescriptor(databaseKey{string(n.n.Name)}, &desc, n.n.IfNotExists)
	if err != nil {
		return err
	}
	if created {
		// Log Create Database event. This is an auditable log event and is
		// recorded in the same transaction as the table descriptor update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogCreateDatabase,
			int32(desc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				DatabaseName string
				Statement    string
				User         string
			}{n.n.Name.String(), n.n.String(), n.p.session.User},
		); err != nil {
			return err
		}
	}
	return nil
}

func (n *createDatabaseNode) Next() (bool, error)                 { return false, nil }
func (n *createDatabaseNode) Columns() []ResultColumn             { return make([]ResultColumn, 0) }
func (n *createDatabaseNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createDatabaseNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createDatabaseNode) DebugValues() debugValues            { return debugValues{} }
func (n *createDatabaseNode) ExplainTypes(_ func(string, string)) {}
func (n *createDatabaseNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createDatabaseNode) MarkDebug(mode explainMode)          {}
func (n *createDatabaseNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create database", "", nil
}

type createIndexNode struct {
	p         *planner
	n         *parser.CreateIndex
	tableDesc *sqlbase.TableDescriptor
}

// CreateIndex creates an index.
// Privileges: CREATE on table.
//   notes: postgres requires CREATE on the table.
//          mysql requires INDEX on the table.
func (p *planner) CreateIndex(n *parser.CreateIndex) (planNode, error) {
	tableDesc, err := p.mustGetTableDesc(n.Table)
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(tableDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createIndexNode{p: p, tableDesc: tableDesc, n: n}, nil
}

func (n *createIndexNode) expandPlan() error {
	return nil
}

func (n *createIndexNode) Start() error {
	status, i, err := n.tableDesc.FindIndexByName(string(n.n.Name))
	if err == nil {
		if status == sqlbase.DescriptorIncomplete {
			switch n.tableDesc.Mutations[i].Direction {
			case sqlbase.DescriptorMutation_DROP:
				return fmt.Errorf("index %q being dropped, try again later", string(n.n.Name))

			case sqlbase.DescriptorMutation_ADD:
				// Noop, will fail in AllocateIDs below.
			}
		}
		if n.n.IfNotExists {
			return nil
		}
	}

	indexDesc := sqlbase.IndexDescriptor{
		Name:             string(n.n.Name),
		Unique:           n.n.Unique,
		StoreColumnNames: n.n.Storing,
	}
	if err := indexDesc.FillColumns(n.n.Columns); err != nil {
		return err
	}

	mutationIdx := len(n.tableDesc.Mutations)
	n.tableDesc.AddIndexMutation(indexDesc, sqlbase.DescriptorMutation_ADD)
	mutationID, err := n.tableDesc.FinalizeMutation()
	if err != nil {
		return err
	}
	if err := n.tableDesc.AllocateIDs(); err != nil {
		return err
	}

	if n.n.Interleave != nil {
		index := n.tableDesc.Mutations[mutationIdx].GetIndex()
		if err := n.p.addInterleave(n.tableDesc, index, n.n.Interleave); err != nil {
			return err
		}
		if err := n.p.finalizeInterleave(n.tableDesc, *index); err != nil {
			return err
		}
	}

	if err := n.p.txn.Put(
		sqlbase.MakeDescMetadataKey(n.tableDesc.GetID()),
		sqlbase.WrapDescriptor(n.tableDesc)); err != nil {
		return err
	}

	// Record index creation in the event log. This is an auditable log
	// event and is recorded in the same transaction as the table descriptor
	// update.
	if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
		EventLogCreateIndex,
		int32(n.tableDesc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			TableName  string
			IndexName  string
			Statement  string
			User       string
			MutationID uint32
		}{n.tableDesc.Name, n.n.Name.String(), n.n.String(), n.p.session.User, uint32(mutationID)},
	); err != nil {
		return err
	}
	n.p.notifySchemaChange(n.tableDesc.ID, mutationID)

	return nil
}

func (n *createIndexNode) Next() (bool, error)                 { return false, nil }
func (n *createIndexNode) Columns() []ResultColumn             { return make([]ResultColumn, 0) }
func (n *createIndexNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createIndexNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createIndexNode) DebugValues() debugValues            { return debugValues{} }
func (n *createIndexNode) ExplainTypes(_ func(string, string)) {}
func (n *createIndexNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createIndexNode) MarkDebug(mode explainMode)          {}
func (n *createIndexNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create index", "", nil
}

type createTableNode struct {
	p      *planner
	n      *parser.CreateTable
	dbDesc *sqlbase.DatabaseDescriptor
}

// CreateTable creates a table.
// Privileges: CREATE on database.
//   Notes: postgres/mysql require CREATE on database.
func (p *planner) CreateTable(n *parser.CreateTable) (planNode, error) {
	if err := n.Table.NormalizeTableName(p.session.Database); err != nil {
		return nil, err
	}

	dbDesc, err := p.mustGetDatabaseDesc(n.Table.Database())
	if err != nil {
		return nil, err
	}

	if err := p.checkPrivilege(dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	return &createTableNode{p: p, n: n, dbDesc: dbDesc}, nil
}

func hoistConstraints(n *parser.CreateTable) {
	for _, d := range n.Defs {
		if col, ok := d.(*parser.ColumnTableDef); ok {
			if col.CheckExpr.Expr != nil {
				def := &parser.CheckConstraintTableDef{Expr: col.CheckExpr.Expr}
				if col.CheckExpr.ConstraintName != "" {
					def.Name = parser.Name(col.CheckExpr.ConstraintName)
				}
				n.Defs = append(n.Defs, def)
				col.CheckExpr.Expr = nil
			}
		}
	}
}

func (n *createTableNode) expandPlan() error {
	return nil
}

func (n *createTableNode) Start() error {
	hoistConstraints(n.n)
	desc, err := sqlbase.MakeTableDesc(n.n, n.dbDesc.ID)
	if err != nil {
		return err
	}
	// Inherit permissions from the database descriptor.
	desc.Privileges = n.dbDesc.GetPrivileges()

	if len(desc.PrimaryIndex.ColumnNames) == 0 {
		// Ensure a Primary Key exists.
		s := "unique_rowid()"
		col := sqlbase.ColumnDescriptor{
			Name: "rowid",
			Type: sqlbase.ColumnType{
				Kind: sqlbase.ColumnType_INT,
			},
			DefaultExpr: &s,
			Hidden:      true,
			Nullable:    false,
		}
		desc.AddColumn(col)
		idx := sqlbase.IndexDescriptor{
			Unique:           true,
			ColumnNames:      []string{col.Name},
			ColumnDirections: []sqlbase.IndexDescriptor_Direction{sqlbase.IndexDescriptor_ASC},
		}
		if err := desc.AddIndex(idx, true); err != nil {
			return err
		}
	}

	if err := desc.AllocateIDs(); err != nil {
		return err
	}

	if n.n.Interleave != nil {
		if err := n.p.addInterleave(&desc, &desc.PrimaryIndex, n.n.Interleave); err != nil {
			return err
		}
	}

	// FKs are resolved after the descriptor is otherwise complete and IDs have
	// been allocated since the FKs will reference those IDs.
	var fkTargets []fkTargetUpdate
	for _, def := range n.n.Defs {
		if col, ok := def.(*parser.ColumnTableDef); ok {
			if col.References.Table != nil {
				modified, err := n.resolveColFK(&desc, col.Name, col.References.Table, col.References.Col, col.References.ConstraintName)
				if err != nil {
					return err
				}
				fkTargets = append(fkTargets, modified)
			}
		}
	}

	// We need to validate again after adding the FKs, but the desc still doesn't
	// have a valid ID, so we briefly set it to something to get past validation.
	savedID := desc.ID
	if desc.ID == 0 {
		desc.ID = keys.MaxReservedDescID + 1
	}
	err = desc.Validate()
	desc.ID = savedID
	if err != nil {
		return err
	}

	created, err := n.p.createDescriptor(
		tableKey{n.dbDesc.ID, n.n.Table.Table()}, &desc, n.n.IfNotExists)
	if err != nil {
		return err
	}

	if err := n.finalizeFKs(&desc, fkTargets); err != nil {
		return err
	}

	for _, index := range desc.AllNonDropIndexes() {
		if len(index.Interleave.Ancestors) > 0 {
			if err := n.p.finalizeInterleave(&desc, index); err != nil {
				return err
			}
		}
	}

	if created {
		// Log Create Table event. This is an auditable log event and is
		// recorded in the same transaction as the table descriptor update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogCreateTable,
			int32(desc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				TableName string
				Statement string
				User      string
			}{n.n.Table.String(), n.n.String(), n.p.session.User},
		); err != nil {
			return err
		}
	}

	return nil
}

func (n *createTableNode) Next() (bool, error)                 { return false, nil }
func (n *createTableNode) Columns() []ResultColumn             { return make([]ResultColumn, 0) }
func (n *createTableNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *createTableNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *createTableNode) DebugValues() debugValues            { return debugValues{} }
func (n *createTableNode) ExplainTypes(_ func(string, string)) {}
func (n *createTableNode) SetLimitHint(_ int64, _ bool)        {}
func (n *createTableNode) MarkDebug(mode explainMode)          {}
func (n *createTableNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "create table", "", nil
}

// FK resolution runs before the referencing (child) table is created, meaning
// its ID, which needs to be noted on the referenced tables, is not yet
// determined. This struct accumulates the information needed to edit a
// referenced table after the referencing table is created and has an ID.
type fkTargetUpdate struct {
	srcIdx    sqlbase.IndexID          // ID of source (referencing) index
	target    *sqlbase.TableDescriptor // Table to update
	targetIdx sqlbase.IndexID          // ID of target (referenced) index
}

func (n *createTableNode) resolveColFK(
	tbl *sqlbase.TableDescriptor,
	fromCol parser.Name,
	targetTable *parser.QualifiedName,
	targetColName parser.Name,
	constraintName parser.Name,
) (fkTargetUpdate, error) {
	var ret fkTargetUpdate
	src, err := tbl.FindActiveColumnByName(string(fromCol))
	if err != nil {
		return ret, err
	}

	target, err := n.p.getTableDesc(targetTable)
	if err != nil {
		return ret, err
	}
	if target == nil {
		if targetTable.String() == tbl.Name {
			target = tbl
		} else {
			return ret, fmt.Errorf("referenced table %q not found", targetTable.String())
		}
	}
	ret.target = target
	// If a column isn't specified, attempt to default to PK.
	if targetColName == "" {
		if len(target.PrimaryIndex.ColumnNames) != 1 {
			return ret, errors.Errorf("must specify a single unique column to reference %q", targetTable.String())
		}
		targetColName = parser.Name(target.PrimaryIndex.ColumnNames[0])
	}

	targetCol, err := target.FindActiveColumnByName(string(targetColName))
	if err != nil {
		return ret, err
	}

	if src.Type.Kind != targetCol.Type.Kind {
		return ret, fmt.Errorf("type of %q (%s) does not match foreign key %q.%q (%s)",
			fromCol, src.Type.Kind, target.Name, targetCol.Name, targetCol.Type.Kind)
	}

	found := false
	if target.PrimaryIndex.ColumnIDs[0] == targetCol.ID {
		found = true
		ret.targetIdx = target.PrimaryIndex.ID
	} else {
		// Find the index corresponding to the referenced column.
		for _, idx := range target.Indexes {
			if idx.Unique && idx.ColumnIDs[0] == targetCol.ID {
				ret.targetIdx = idx.ID
				found = true
				break
			}
		}
	}
	if !found {
		return ret, fmt.Errorf("foreign key requires a unique index on %s.%s", targetTable.String(), targetCol.Name)
	}

	if constraintName == "" {
		constraintName = parser.Name(fmt.Sprintf("fk_%s_ref_%s_%s", fromCol, target.Name, targetColName))
	}

	ref := &sqlbase.ForeignKeyReference{Table: target.ID, Index: ret.targetIdx, Name: string(constraintName)}

	found = false
	if tbl.PrimaryIndex.ColumnIDs[0] == src.ID {
		tbl.PrimaryIndex.ForeignKey = ref
		ret.srcIdx = tbl.PrimaryIndex.ID
		found = true
	} else {
		for i, idx := range tbl.Indexes {
			if tbl.Indexes[i].ColumnIDs[0] == src.ID {
				tbl.Indexes[i].ForeignKey = ref
				ret.srcIdx = idx.ID
				found = true
				break
			}
		}
	}
	if !found {
		return ret, fmt.Errorf("foreign key column %q must be the prefix of an index", src.Name)
	}

	tbl.State = sqlbase.TableDescriptor_ADD
	return ret, nil
}

func (p *planner) saveNonmutationAndNotify(td *sqlbase.TableDescriptor) error {
	if err := td.SetUpVersion(); err != nil {
		return err
	}
	if err := td.Validate(); err != nil {
		return err
	}
	if err := p.writeTableDesc(td); err != nil {
		return err
	}
	p.notifySchemaChange(td.ID, sqlbase.InvalidMutationID)
	return nil
}

// addInterleave marks an index as one that is interleaved in some parent data
// according to the given definition.
func (p *planner) addInterleave(
	desc *sqlbase.TableDescriptor, index *sqlbase.IndexDescriptor, interleave *parser.InterleaveDef,
) error {
	if interleave.DropBehavior != parser.DropDefault {
		return util.UnimplementedWithIssueErrorf(
			7854, "unsupported shorthand %s", interleave.DropBehavior)
	}

	parentTable, err := p.mustGetTableDesc(interleave.Parent)
	if err != nil {
		return err
	}
	parentIndex := parentTable.PrimaryIndex

	if len(interleave.Fields) != len(parentIndex.ColumnIDs) {
		return fmt.Errorf("interleaved columns must match parent")
	}
	if len(interleave.Fields) > len(index.ColumnIDs) {
		return fmt.Errorf("declared columns must match index being interleaved")
	}
	for i, targetColID := range parentIndex.ColumnIDs {
		targetCol, err := parentTable.FindColumnByID(targetColID)
		if err != nil {
			return err
		}
		col, err := desc.FindColumnByID(index.ColumnIDs[i])
		if err != nil {
			return err
		}
		if sqlbase.NormalizeName(interleave.Fields[i]) != sqlbase.NormalizeName(string(col.Name)) {
			return fmt.Errorf("declared columns must match index being interleaved")
		}
		if col.Type != targetCol.Type ||
			index.ColumnDirections[i] != parentIndex.ColumnDirections[i] {

			return fmt.Errorf("interleaved columns must match parent")
		}
	}

	ancestorPrefix := append(
		[]sqlbase.InterleaveDescriptor_Ancestor(nil), parentIndex.Interleave.Ancestors...)
	intl := sqlbase.InterleaveDescriptor_Ancestor{
		TableID:         parentTable.ID,
		IndexID:         parentIndex.ID,
		SharedPrefixLen: uint32(len(parentIndex.ColumnIDs)),
	}
	for _, ancestor := range ancestorPrefix {
		intl.SharedPrefixLen -= uint32(ancestor.SharedPrefixLen)
	}
	index.Interleave = sqlbase.InterleaveDescriptor{Ancestors: append(ancestorPrefix, intl)}

	return nil
}

func (n *createTableNode) finalizeFKs(desc *sqlbase.TableDescriptor, fkTargets []fkTargetUpdate) error {
	for _, t := range fkTargets {
		targetIdx, err := t.target.FindIndexByID(t.targetIdx)
		if err != nil {
			return err
		}
		targetIdx.ReferencedBy = append(targetIdx.ReferencedBy,
			&sqlbase.ForeignKeyReference{Table: desc.ID, Index: t.srcIdx})

		if t.target == desc {
			srcIdx, err := desc.FindIndexByID(t.srcIdx)
			if err != nil {
				return err
			}
			srcIdx.ForeignKey.Table = desc.ID
			continue
		}

		// TODO(dt): Only save each referenced table once.
		if err := n.p.saveNonmutationAndNotify(t.target); err != nil {
			return err
		}
	}

	if desc.State == sqlbase.TableDescriptor_ADD {
		desc.State = sqlbase.TableDescriptor_PUBLIC

		if err := n.p.saveNonmutationAndNotify(desc); err != nil {
			return err
		}
	}
	return nil
}

// finalizeInterleave creats backreferences from an interleaving parent to the
// child data being interleaved.
func (p *planner) finalizeInterleave(
	desc *sqlbase.TableDescriptor, index sqlbase.IndexDescriptor,
) error {
	// TODO(dan): This is similar to finalizeFKs. Consolidate them.
	for _, ancestor := range index.Interleave.Ancestors {
		ancestorTable, err := getTableDescFromID(p.txn, ancestor.TableID)
		if err != nil {
			return err
		}
		ancestorIndex, err := ancestorTable.FindIndexByID(ancestor.IndexID)
		if err != nil {
			return err
		}
		ancestorIndex.InterleavedBy = append(ancestorIndex.InterleavedBy,
			sqlbase.ForeignKeyReference{Table: desc.ID, Index: index.ID})

		// TODO(dan): Only save each referenced table once.
		if err := p.saveNonmutationAndNotify(ancestorTable); err != nil {
			return err
		}
	}
	return nil
}
