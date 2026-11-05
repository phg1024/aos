#include "textflag.h"

// func int80(nr, a, b, c uintptr) uintptr
TEXT ·int80(SB),NOSPLIT,$0-40
	MOVQ nr+0(FP), AX
	MOVQ a+8(FP), BX
	MOVQ b+16(FP), CX
	MOVQ c+24(FP), DX
	INT $0x80
	MOVQ AX, ret+32(FP)
	RET
