// Package lotsize defines the fixed conversion between a user-facing order
// "lot" and the underlying quantity used in margin and PnL math.
package lotsize

// Size is the number of underlying units one lot represents. An order or
// position quantity of N lots is worth N*Size economically.
const Size = 25.0
