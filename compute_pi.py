from decimal import Decimal, getcontext
import math

def compute_pi_chudnovsky(digits=300):
    """Compute pi to specified digits using Chudnovsky algorithm."""
    # Set precision with buffer
    getcontext().prec = digits + 50
    
    # Constants
    C = Decimal(640320)
    C_sqrt = C.sqrt() ** 3
    M = Decimal(13591409)
    R = Decimal(545140134)
    K = Decimal(12)
    
    # Initialize sum
    total = Decimal(0)
    k = 0
    
    while True:
        # Compute factorials as integers for precision
        fact_6k = math.factorial(6 * k)
        fact_3k = math.factorial(3 * k)
        fact_k = math.factorial(k)
        
        # Numerator: (-1)^k * (6k)! * (13591409 + 545140134k)
        numerator = ((-1) ** k) * fact_6k * (M + R * k)
        
        # Denominator: (3k)! * (k!)^3 * (640320)^(3k + 3/2)
        denominator = fact_3k * (fact_k ** 3) * (C ** (3 * k)) * C_sqrt
        
        # Term
        term = Decimal(numerator) / Decimal(denominator)
        total += term
        
        # Check convergence
        if abs(term) < Decimal(10) ** (-(digits + 10)):
            break
        
        k += 1
        if k > 100:  # Safety limit
            break
    
    # pi = 1 / (12 * sum)
    pi = K / total
    return +pi  # Apply precision

if __name__ == "__main__":
    print("Computing pi to 300 digits using Chudnovsky algorithm...")
    pi_300 = compute_pi_chudnovsky(300)
    print(f"\nπ ≈ {pi_300}")
    print(f"\nFirst 300 digits:")
    print(str(pi_300)[:301])
