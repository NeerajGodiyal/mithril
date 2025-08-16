package sbpf

// Op classes
const (
	ClassLd = uint8(iota)
	ClassLdx
	ClassSt
	ClassStx
	ClassAlu
	ClassJmp
	ClassPqr
	ClassAlu64
)

// Size modes
const (
	SizeW = uint8(iota * 0x08)
	SizeH
	SizeB
	SizeDw
	Size1B = 0x20
	Size2B = 0x30
	Size4B = 0x80
	Size8B = 0x90
)

// Addressing modes
const (
	AddrImm = uint8(iota * 0x20)
	AddrAbs
	AddrInd
	AddrMem
	Addr0x80 // reserved
	Addr0xa0 // reserved
	AddrXadd
)

// Source modes
const (
	SrcK = uint8(iota * 0x08)
	SrcX
)

// ALU operations
const (
	AluAdd = uint8(iota * 0x10)
	AluSub
	AluMul
	AluDiv
	AluOr
	AluAnd
	AluLsh
	AluRsh
	AluNeg
	AluMod
	AluXor
	AluMov
	AluArsh
	AluEnd
	AluSdiv
	AluHor
)

// Jump operations
const (
	JumpAlways = uint8(iota * 0x10)
	JumpEq
	JumpGt
	JumpGe
	JumpSet
	JumpNe
	JumpSgt
	JumpSge
	JumpCall
	JumpExit
	JumpLt
	JumpLe
	JumpSlt
	JumpSle
)

// PQR operations
const (
	PqrUhmul = uint8(0x20)
	PqrUdiv  = uint8(0x40)
	PqrUrem  = uint8(0x60)
	PqrLmul  = uint8(0x80)
	PqrShmul = uint8(0xa0)
	PqrSdiv  = uint8(0xc0)
	PqrSrem  = uint8(0xe0)
)

// Opcodes
const (
	OpLddw = ClassLd | AddrImm | SizeDw

	OpLdxb  = ClassLdx | AddrMem | SizeB
	OpLdxh  = ClassLdx | AddrMem | SizeH
	OpLdxw  = ClassLdx | AddrMem | SizeW
	OpLdxdw = ClassLdx | AddrMem | SizeDw
	OpStb   = ClassSt | AddrMem | SizeB
	OpSth   = ClassSt | AddrMem | SizeH
	OpStw   = ClassSt | AddrMem | SizeW
	OpStdw  = ClassSt | AddrMem | SizeDw
	OpStxb  = ClassStx | AddrMem | SizeB
	OpStxh  = ClassStx | AddrMem | SizeH
	OpStxw  = ClassStx | AddrMem | SizeW
	OpStxdw = ClassStx | AddrMem | SizeDw

	OpAdd32Imm  = ClassAlu | SrcK | AluAdd
	OpAdd32Reg  = ClassAlu | SrcX | AluAdd
	OpSub32Imm  = ClassAlu | SrcK | AluSub
	OpSub32Reg  = ClassAlu | SrcX | AluSub
	OpMul32Imm  = ClassAlu | SrcK | AluMul
	OpMul32Reg  = ClassAlu | SrcX | AluMul
	OpDiv32Imm  = ClassAlu | SrcK | AluDiv
	OpDiv32Reg  = ClassAlu | SrcX | AluDiv
	OpOr32Imm   = ClassAlu | SrcK | AluOr
	OpOr32Reg   = ClassAlu | SrcX | AluOr
	OpAnd32Imm  = ClassAlu | SrcK | AluAnd
	OpAnd32Reg  = ClassAlu | SrcX | AluAnd
	OpLsh32Imm  = ClassAlu | SrcK | AluLsh
	OpLsh32Reg  = ClassAlu | SrcX | AluLsh
	OpRsh32Imm  = ClassAlu | SrcK | AluRsh
	OpRsh32Reg  = ClassAlu | SrcX | AluRsh
	OpNeg32     = ClassAlu | AluNeg
	OpMod32Imm  = ClassAlu | SrcK | AluMod
	OpMod32Reg  = ClassAlu | SrcX | AluMod
	OpXor32Imm  = ClassAlu | SrcK | AluXor
	OpXor32Reg  = ClassAlu | SrcX | AluXor
	OpMov32Imm  = ClassAlu | SrcK | AluMov
	OpMov32Reg  = ClassAlu | SrcX | AluMov
	OpArsh32Imm = ClassAlu | SrcK | AluArsh
	OpArsh32Reg = ClassAlu | SrcX | AluArsh
	OpLe        = ClassAlu | SrcK | AluEnd
	OpBe        = ClassAlu | SrcX | AluEnd

	OpAdd64Imm  = ClassAlu64 | SrcK | AluAdd
	OpAdd64Reg  = ClassAlu64 | SrcX | AluAdd
	OpSub64Imm  = ClassAlu64 | SrcK | AluSub
	OpSub64Reg  = ClassAlu64 | SrcX | AluSub
	OpMul64Imm  = ClassAlu64 | SrcK | AluMul
	OpMul64Reg  = ClassAlu64 | SrcX | AluMul
	OpDiv64Imm  = ClassAlu64 | SrcK | AluDiv
	OpDiv64Reg  = ClassAlu64 | SrcX | AluDiv
	OpOr64Imm   = ClassAlu64 | SrcK | AluOr
	OpOr64Reg   = ClassAlu64 | SrcX | AluOr
	OpAnd64Imm  = ClassAlu64 | SrcK | AluAnd
	OpAnd64Reg  = ClassAlu64 | SrcX | AluAnd
	OpLsh64Imm  = ClassAlu64 | SrcK | AluLsh
	OpLsh64Reg  = ClassAlu64 | SrcX | AluLsh
	OpRsh64Imm  = ClassAlu64 | SrcK | AluRsh
	OpRsh64Reg  = ClassAlu64 | SrcX | AluRsh
	OpNeg64     = ClassAlu64 | AluNeg
	OpMod64Imm  = ClassAlu64 | SrcK | AluMod
	OpMod64Reg  = ClassAlu64 | SrcX | AluMod
	OpXor64Imm  = ClassAlu64 | SrcK | AluXor
	OpXor64Reg  = ClassAlu64 | SrcX | AluXor
	OpMov64Imm  = ClassAlu64 | SrcK | AluMov
	OpMov64Reg  = ClassAlu64 | SrcX | AluMov
	OpArsh64Imm = ClassAlu64 | SrcK | AluArsh
	OpArsh64Reg = ClassAlu64 | SrcX | AluArsh
	OpHor64Imm  = ClassAlu64 | SrcK | AluHor

	OpJa      = ClassJmp | JumpAlways
	OpJeqImm  = ClassJmp | SrcK | JumpEq
	OpJeqReg  = ClassJmp | SrcX | JumpEq
	OpJgtImm  = ClassJmp | SrcK | JumpGt
	OpJgtReg  = ClassJmp | SrcX | JumpGt
	OpJgeImm  = ClassJmp | SrcK | JumpGe
	OpJgeReg  = ClassJmp | SrcX | JumpGe
	OpJltImm  = ClassJmp | SrcK | JumpLt
	OpJltReg  = ClassJmp | SrcX | JumpLt
	OpJleImm  = ClassJmp | SrcK | JumpLe
	OpJleReg  = ClassJmp | SrcX | JumpLe
	OpJsetImm = ClassJmp | SrcK | JumpSet
	OpJsetReg = ClassJmp | SrcX | JumpSet
	OpJneImm  = ClassJmp | SrcK | JumpNe
	OpJneReg  = ClassJmp | SrcX | JumpNe
	OpJsgtImm = ClassJmp | SrcK | JumpSgt
	OpJsgtReg = ClassJmp | SrcX | JumpSgt
	OpJsgeImm = ClassJmp | SrcK | JumpSge
	OpJsgeReg = ClassJmp | SrcX | JumpSge
	OpJsltImm = ClassJmp | SrcK | JumpSlt
	OpJsltReg = ClassJmp | SrcX | JumpSlt
	OpJsleImm = ClassJmp | SrcK | JumpSle
	OpJsleReg = ClassJmp | SrcX | JumpSle

	OpCall  = ClassJmp | SrcK | JumpCall
	OpCallx = ClassJmp | SrcX | JumpCall
	OpExit  = ClassJmp | JumpExit

	OpLmul32Imm = ClassPqr | SrcK | PqrLmul
	OpLmul32Reg = ClassPqr | SrcX | PqrLmul
	//OpUhmul32Imm = ClassPqr | SrcK | PqrUhmul
	//OpUhmul32Reg = ClassPqr | SrcX | PqrUhmul
	OpUdiv32Imm = ClassPqr | SrcK | PqrUdiv
	OpUdiv32Reg = ClassPqr | SrcX | PqrUdiv
	OpUrem32Imm = ClassPqr | SrcK | PqrUrem
	OpUrem32Reg = ClassPqr | SrcX | PqrUrem
	//OpShmul32Imm = ClassPqr | SrcK | PqrShmul
	//OpShmul32Reg = ClassPqr | SrcX | PqrShmul
	OpSdiv32Imm = ClassPqr | SrcK | PqrSdiv
	OpSdiv32Reg = ClassPqr | SrcX | PqrSdiv
	OpSrem32Imm = ClassPqr | SrcK | PqrSrem
	OpSrem32Reg = ClassPqr | SrcX | PqrSrem

	OpLmul64Imm  = ClassPqr | SizeB | SrcK | PqrLmul
	OpLmul64Reg  = ClassPqr | SizeB | SrcX | PqrLmul
	OpUhmul64Imm = ClassPqr | SizeB | SrcK | PqrUhmul
	OpUhmul64Reg = ClassPqr | SizeB | SrcX | PqrUhmul
	OpUdiv64Imm  = ClassPqr | SizeB | SrcK | PqrUdiv
	OpUdiv64Reg  = ClassPqr | SizeB | SrcX | PqrUdiv
	OpUrem64Imm  = ClassPqr | SizeB | SrcK | PqrUrem
	OpUrem64Reg  = ClassPqr | SizeB | SrcX | PqrUrem
	OpShmul64Imm = ClassPqr | SizeB | SrcK | PqrShmul
	OpShmul64Reg = ClassPqr | SizeB | SrcX | PqrShmul
	OpSdiv64Imm  = ClassPqr | SizeB | SrcK | PqrSdiv
	OpSdiv64Reg  = ClassPqr | SizeB | SrcX | PqrSdiv
	OpSrem64Imm  = ClassPqr | SizeB | SrcK | PqrSrem
	OpSrem64Reg  = ClassPqr | SizeB | SrcX | PqrSrem

	OpLd1BReg = ClassAlu | SrcX | Size1B
	OpLd2BReg = ClassAlu | SrcX | Size2B
	OpLd4BReg = ClassAlu | SrcX | Size4B
	OpLd8BReg = ClassAlu | SrcX | Size8B
	OpSt1BImm = ClassAlu64 | SrcK | Size1B
	OpSt2BImm = ClassAlu64 | SrcK | Size2B
	OpSt4BImm = ClassAlu64 | SrcK | Size4B
	OpSt8BImm = ClassAlu64 | SrcK | Size8B
	OpSt1BReg = ClassAlu64 | SrcX | Size1B
	OpSt2BReg = ClassAlu64 | SrcX | Size2B
	OpSt4BReg = ClassAlu64 | SrcX | Size4B
	OpSt8BReg = ClassAlu64 | SrcX | Size8B
)
